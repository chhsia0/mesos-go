package httpsched

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/mesos/mesos-go"
	"github.com/mesos/mesos-go/backoff"
	"github.com/mesos/mesos-go/encoding"
	"github.com/mesos/mesos-go/httpcli"
	"github.com/mesos/mesos-go/scheduler"
)

const (
	headerMesosStreamID        = "Mesos-Stream-Id"
	debug                      = false
	defaultMaxRedirectAttempts = 9 // per-Do invocation
)

var (
	errMissingMesosStreamId = errors.New("missing Mesos-Stream-Id header expected with successful SUBSCRIBE")
	errNotHTTP              = errors.New("expected an HTTP object, found something else instead")

	// MinRedirectBackoffPeriod MUST be set to some non-zero number, otherwise redirects will panic
	MinRedirectBackoffPeriod = 100 * time.Millisecond
	// MaxRedirectBackoffPeriod SHOULD be set to a value greater than MinRedirectBackoffPeriod
	MaxRedirectBackoffPeriod = 13 * time.Second
)

type (
	withTemporary interface {
		// WithTemporary configures the Client with the temporary option and returns the results of
		// invoking f(). Changes made to the Client by the temporary option are reverted before this
		// func returns.
		WithTemporary(opt httpcli.Opt, f func() error) error
	}

	// httpClient is the Mesos transport layer client, for internal use
	httpClient interface {
		withTemporary
		Endpoint() string
		Do(encoding.Marshaler, ...httpcli.RequestOpt) (mesos.Response, error)
		With(...httpcli.Opt) httpcli.Opt
	}

	client struct {
		httpClient
		maxRedirects int
	}

	// Client is the public interface ths framework scheduler's should consume
	Client interface {
		withTemporary
		// CallNoData is for scheduler calls that are not expected to return any data from the server.
		CallNoData(*scheduler.Call) error
		// Call issues a call to Mesos and properly manages call-specific HTTP response headers & data.
		Call(*scheduler.Call) (mesos.Response, httpcli.Opt, error)
	}

	Option func(*client) Option
)

// MaxRedirects is a functional option that sets the maximum number of per-call HTTP redirects for a scheduler client
func MaxRedirects(mr int) Option {
	return func(c *client) Option {
		old := c.maxRedirects
		c.maxRedirects = mr
		return MaxRedirects(old)
	}
}

// NewClient returns a scheduler API Client
func NewClient(cl *httpcli.Client, opts ...Option) Client {
	result := &client{httpClient: cl, maxRedirects: defaultMaxRedirectAttempts}
	for _, o := range opts {
		if o != nil {
			o(result)
		}
	}
	return result
}

// CallNoData implements Client
func (cli *client) CallNoData(call *scheduler.Call) error {
	resp, err := cli.callWithRedirect(func() (mesos.Response, error) {
		return cli.Do(call)
	})
	if resp != nil {
		resp.Close()
	}
	return err
}

// Call implements Client
func (cli *client) Call(call *scheduler.Call) (resp mesos.Response, maybeOpt httpcli.Opt, err error) {
	opt, requestOpt, optGen := cli.prepare(call)
	cli.WithTemporary(opt, func() error {
		resp, err = cli.callWithRedirect(func() (mesos.Response, error) {
			return cli.Do(call, requestOpt...)
		})
		return nil
	})
	maybeOpt = optGen()
	return
}

var defaultOptGen = func() httpcli.Opt { return nil }

// prepare is invoked for scheduler call's that require pre-processing, post-processing, or both.
func (cli *client) prepare(call *scheduler.Call) (undoable httpcli.Opt, requestOpt []httpcli.RequestOpt, optGen func() httpcli.Opt) {
	optGen = defaultOptGen
	switch call.GetType() {
	case scheduler.Call_SUBSCRIBE:
		mesosStreamID := ""
		undoable = httpcli.WrapDoer(func(f httpcli.DoFunc) httpcli.DoFunc {
			return func(req *http.Request) (*http.Response, error) {
				if debug {
					log.Println("wrapping request")
				}
				resp, err := f(req)
				if debug && err == nil {
					log.Printf("status %d", resp.StatusCode)
					for k := range resp.Header {
						log.Println("header " + k + ": " + resp.Header.Get(k))
					}
				}
				if err == nil && resp.StatusCode == 200 {
					// grab Mesos-Stream-Id header; if missing then
					// close the response body and return an error
					mesosStreamID = resp.Header.Get(headerMesosStreamID)
					if mesosStreamID == "" {
						resp.Body.Close()
						return nil, errMissingMesosStreamId
					}
					if debug {
						log.Println("found mesos-stream-id: " + mesosStreamID)
					}
				}
				return resp, err
			}
		})
		requestOpt = []httpcli.RequestOpt{httpcli.Close(true)}
		optGen = func() httpcli.Opt { return httpcli.DefaultHeader(headerMesosStreamID, mesosStreamID) }
	default:
		// there are no other, special calls that generate data and require pre/post processing
	}
	return
}

func (cli *client) callWithRedirect(f func() (mesos.Response, error)) (resp mesos.Response, err error) {
	var (
		done            chan struct{} // avoid allocating these chans unless we actually need to redirect
		redirectBackoff <-chan struct{}
		getBackoff      = func() <-chan struct{} {
			if redirectBackoff == nil {
				done = make(chan struct{})
				redirectBackoff = backoff.Notifier(MinRedirectBackoffPeriod, MaxRedirectBackoffPeriod, done)
			}
			return redirectBackoff
		}
	)
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	for attempt := 0; ; attempt++ {
		resp, err = f()
		if err == nil || (err != nil && err != httpcli.ErrNotLeader) {
			return resp, err
		}
		res, ok := resp.(*httpcli.Response)
		if !ok {
			if resp != nil {
				resp.Close()
			}
			return nil, errNotHTTP
		}
		// TODO(jdef) refactor this
		// mesos v0.29 will actually send back fully-formed URLs in the Location header
		log.Println("master changed?")
		if attempt < cli.maxRedirects {
			location, ok := buildNewEndpoint(res.Header.Get("Location"), cli.Endpoint())
			if !ok {
				return
			}
			res.Close()
			log.Println("redirecting to " + location)
			cli.With(httpcli.Endpoint(location))
			<-getBackoff()
			continue
		}
		return
	}
}

func buildNewEndpoint(location, currentEndpoint string) (string, bool) {
	if location == "" {
		return "", false
	}
	// current format appears to be //x.y.z.w:port
	hostport, parseErr := url.Parse(location)
	if parseErr != nil || hostport.Host == "" {
		return "", false
	}
	current, parseErr := url.Parse(currentEndpoint)
	if parseErr != nil {
		return "", false
	}
	current.Host = hostport.Host
	return current.String(), true
}