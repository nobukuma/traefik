package middlewares

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containous/traefik/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/roundrobin"
)

func TestRetry(t *testing.T) {
	testCases := []struct {
		desc                        string
		maxRequestAttempts          int
		wantRetryAttempts           int
		wantResponseStatus          int
		amountFaultyEndpoints       int
		isWebsocketHandshakeRequest bool
	}{
		{
			desc:                  "no retry on success",
			maxRequestAttempts:    1,
			wantRetryAttempts:     0,
			wantResponseStatus:    http.StatusOK,
			amountFaultyEndpoints: 0,
		},
		{
			desc:                  "no retry when max request attempts is one",
			maxRequestAttempts:    1,
			wantRetryAttempts:     0,
			wantResponseStatus:    http.StatusInternalServerError,
			amountFaultyEndpoints: 1,
		},
		{
			desc:                  "one retry when one server is faulty",
			maxRequestAttempts:    2,
			wantRetryAttempts:     1,
			wantResponseStatus:    http.StatusOK,
			amountFaultyEndpoints: 1,
		},
		{
			desc:                  "two retries when two servers are faulty",
			maxRequestAttempts:    3,
			wantRetryAttempts:     2,
			wantResponseStatus:    http.StatusOK,
			amountFaultyEndpoints: 2,
		},
		{
			desc:                  "max attempts exhausted delivers the 5xx response",
			maxRequestAttempts:    3,
			wantRetryAttempts:     2,
			wantResponseStatus:    http.StatusInternalServerError,
			amountFaultyEndpoints: 3,
		},
		{
			desc:                        "websocket request should not be retried",
			maxRequestAttempts:          3,
			wantRetryAttempts:           0,
			wantResponseStatus:          http.StatusBadGateway,
			amountFaultyEndpoints:       1,
			isWebsocketHandshakeRequest: true,
		},
	}

	backendServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte("OK"))
	}))

	forwarder, err := forward.New()
	if err != nil {
		t.Fatalf("Error creating forwarder: %s", err)
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			loadBalancer, err := roundrobin.New(forwarder)
			if err != nil {
				t.Fatalf("Error creating load balancer: %s", err)
			}

			basePort := 33444
			for i := 0; i < tc.amountFaultyEndpoints; i++ {
				// 192.0.2.0 is a non-routable IP for testing purposes.
				// See: https://stackoverflow.com/questions/528538/non-routable-ip-address/18436928#18436928
				// We only use the port specification here because the URL is used as identifier
				// in the load balancer and using the exact same URL would not add a new server.
				err = loadBalancer.UpsertServer(testhelpers.MustParseURL("http://192.0.2.0:" + string(basePort+i)))
				assert.NoError(t, err)
			}

			// add the functioning server to the end of the load balancer list
			err = loadBalancer.UpsertServer(testhelpers.MustParseURL(backendServer.URL))
			assert.NoError(t, err)

			retryListener := &countingRetryListener{}
			retry := NewRetry(tc.maxRequestAttempts, loadBalancer, retryListener)

			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://localhost:3000/ok", nil)

			if tc.isWebsocketHandshakeRequest {
				req.Header.Add("Connection", "Upgrade")
				req.Header.Add("Upgrade", "websocket")
			}

			retry.ServeHTTP(recorder, req)

			if tc.wantResponseStatus != recorder.Code {
				t.Errorf("got status code %d, want %d", recorder.Code, tc.wantResponseStatus)
			}
			if tc.wantRetryAttempts != retryListener.timesCalled {
				t.Errorf("retry listener called %d time(s), want %d time(s)", retryListener.timesCalled, tc.wantRetryAttempts)
			}
		})
	}
}

func TestRetryEmptyServerList(t *testing.T) {
	forwarder, err := forward.New()
	if err != nil {
		t.Fatalf("Error creating forwarder: %s", err)
	}

	loadBalancer, err := roundrobin.New(forwarder)
	if err != nil {
		t.Fatalf("Error creating load balancer: %s", err)
	}

	// The EmptyBackendHandler middleware ensures that there is a 503
	// response status set when there is no backend server in the pool.
	next := NewEmptyBackendHandler(loadBalancer)

	retryListener := &countingRetryListener{}
	retry := NewRetry(3, next, retryListener)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://localhost:3000/ok", nil)

	retry.ServeHTTP(recorder, req)

	const wantResponseStatus = http.StatusServiceUnavailable
	if wantResponseStatus != recorder.Code {
		t.Errorf("got status code %d, want %d", recorder.Code, wantResponseStatus)
	}
	const wantRetryAttempts = 0
	if wantRetryAttempts != retryListener.timesCalled {
		t.Errorf("retry listener called %d time(s), want %d time(s)", retryListener.timesCalled, wantRetryAttempts)
	}
}

func TestRetryListeners(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	retryListeners := RetryListeners{&countingRetryListener{}, &countingRetryListener{}}

	retryListeners.Retried(req, 1)
	retryListeners.Retried(req, 1)

	for _, retryListener := range retryListeners {
		listener := retryListener.(*countingRetryListener)
		if listener.timesCalled != 2 {
			t.Errorf("retry listener was called %d time(s), want %d time(s)", listener.timesCalled, 2)
		}
	}
}

// countingRetryListener is a RetryListener implementation to count the times the Retried fn is called.
type countingRetryListener struct {
	timesCalled int
}

func (l *countingRetryListener) Retried(req *http.Request, attempt int) {
	l.timesCalled++
}

func TestRetryWithFlush(t *testing.T) {
	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
		rw.Write([]byte("FULL "))
		rw.(http.Flusher).Flush()
		rw.Write([]byte("DATA"))
	})

	retry := NewRetry(1, next, &countingRetryListener{})
	responseRecorder := httptest.NewRecorder()

	retry.ServeHTTP(responseRecorder, &http.Request{})

	if responseRecorder.Body.String() != "FULL DATA" {
		t.Errorf("Wrong body %q want %q", responseRecorder.Body.String(), "FULL DATA")
	}
}
