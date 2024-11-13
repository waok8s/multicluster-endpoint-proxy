/*
 */

package controller

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type Proxy struct {
	serverForLocal  *http.Server
	serverForRemote *http.Server
}

//+kubebuilder:rbac:groups=*,resources=*,verbs=*

const (
	CERTCRTFILE = "/etc/multiclusterendpointproxy/pki/tls.crt"
	CERTKEYFILE = "/etc/multiclusterendpointproxy/pki/tls.key"
	ROOTCAFILE  = "/etc/kubernetes/pki/ca.crt"
)

var (
	proxyLog = ctrl.Log.WithName("proxy")
)

func CloneHeader(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for key, values := range in {
		newValues := make([]string, len(values))
		copy(newValues, values)
		out[key] = newValues
	}
	return out
}

type CustomResponseWriter struct {
	body       []byte
	statusCode int
	header     http.Header
}

func (w *CustomResponseWriter) Header() http.Header {
	return w.header
}

func (w *CustomResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *CustomResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

type ProxyHandler1 struct {
	config *rest.Config
	dr     *DistributedRequest
}

func (h *ProxyHandler1) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	proxyLog.Info("PORT1: " + req.Method + " " + req.URL.Path)

	reqBody, _ := io.ReadAll(req.Body)

	responseKind, multiEndpointReq := h.dr.CreateDistributedRequest(req, reqBody)

	//var mutex sync.Mutex
	var wg sync.WaitGroup
	var rwg sync.WaitGroup
	var respBodys [][]byte
	if responseKind != RESP_KIND_NO_MERGE {
		rwg.Add(len(multiEndpointReq) - 1)
	}

	for i, v := range multiEndpointReq {
		var transport http.RoundTripper
		u, _ := url.Parse(v.Host)
		reverseProxyLocation, _ := url.Parse(u.String())
		proxy := httputil.NewSingleHostReverseProxy(reverseProxyLocation)
		proxy.FlushInterval = 200 * time.Millisecond
		var rw http.ResponseWriter
		if i == 0 {
			config := rest.CopyConfig(h.config)
			config.APIPath = "/api"
			transport, _ = rest.TransportFor(config)
			if responseKind != RESP_KIND_NO_MERGE {
				proxy.ModifyResponse = func(r *http.Response) error {
					rwg.Wait()
					if r.StatusCode == http.StatusOK && len(respBodys) > 0 {
						body, _ := io.ReadAll(r.Body)
						var responseBody []byte
						switch responseKind {
						case RESP_KIND_TABLE:
							var table metav1.Table
							json.Unmarshal(body, &table)
							if table.Rows != nil {
								var count int
								for _, b := range respBodys {
									var t metav1.Table
									json.Unmarshal(b, &t)
									if len(t.Rows) > 0 {
										table.Rows = append(table.Rows, t.Rows...)
										count++
									}
								}
								if count > 0 {
									responseBody, _ = json.Marshal(table)
								} else {
									responseBody = body
								}
							} else {
								responseBody = body
							}
						case RESP_KIND_ANY_LIST:
							var anyList AnyList[any]
							json.Unmarshal(body, &anyList)
							if anyList.Items != nil {
								var count int
								for _, b := range respBodys {
									var t AnyList[any]
									json.Unmarshal(b, &t)
									if len(t.Items) > 0 {
										anyList.Items = append(anyList.Items, t.Items...)
										count++
									}
								}
								if count > 0 {
									responseBody, _ = json.Marshal(anyList)
								} else {
									responseBody = body
								}
							} else {
								responseBody = body
							}
						default:
							responseBody = body
						}
						r.Body = io.NopCloser(bytes.NewReader(responseBody))
						r.Header.Set("content-length", strconv.Itoa(len(responseBody)))
						r.ContentLength = int64(len(responseBody))
					}
					return nil
				}
			}
			rw = w
		} else {
			transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			if responseKind != RESP_KIND_NO_MERGE {
				proxy.ModifyResponse = func(r *http.Response) error {
					if r.StatusCode == http.StatusOK {
						b, _ := io.ReadAll(r.Body)
						r.Body.Close()
						if len(b) > 0 {
							respBodys = append(respBodys, b)
							r.Body = io.NopCloser(bytes.NewReader(b))
						}
					}
					rwg.Done()
					return nil
				}
			}
			rw = &CustomResponseWriter{
				header: http.Header{},
			}
			v.Request.SetBasicAuth(os.Getenv("BASIC_AUTH_USERNAME"), os.Getenv("BASIC_AUTH_PASSWORD"))
		}
		proxy.Transport = transport
		wg.Add(1)
		f := func(req *http.Request) {
			proxy.ServeHTTP(rw, req)
			wg.Done()
		}
		go f(v.Request)
	}
	wg.Wait()
}

type ProxyHandler2 struct {
	config *rest.Config
}

func (h *ProxyHandler2) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	proxyLog.Info("PORT2: " + req.Method + " " + req.URL.Path)
	if username, password, ok := req.BasicAuth(); !ok || username != os.Getenv("BASIC_AUTH_USERNAME") || password != os.Getenv("BASIC_AUTH_PASSWORD") {
		w.Header().Add("WWW-Authenticate", `Basic realm="private area"`)
		w.WriteHeader(http.StatusUnauthorized)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	config := rest.CopyConfig(h.config)
	config.APIPath = "/api"

	u, _ := url.Parse(config.Host)

	transport, _ := rest.TransportFor(config)

	newReq := req.WithContext(req.Context())
	newReq.Header = CloneHeader(req.Header)
	newReq.Header.Del("Authorization")
	newReq.Host = u.Host

	reverseProxyLocation, _ := url.Parse(u.String())
	proxy := httputil.NewSingleHostReverseProxy(reverseProxyLocation)

	proxy.Transport = transport
	proxy.FlushInterval = 200 * time.Millisecond
	proxy.ServeHTTP(w, newReq)
}

func NewProxy(mgr manager.Manager) *Proxy {
	mux1 := http.NewServeMux()
	mgr.GetCache()
	mux1.Handle("/", http.Handler(&ProxyHandler1{
		config: mgr.GetConfig(),
		dr:     NewDistributedRequest(mgr),
	}))
	server1 := http.Server{
		Handler: mux1,
	}
	mux2 := http.NewServeMux()
	mux2.Handle("/", http.Handler(&ProxyHandler2{
		config: mgr.GetConfig(),
	}))
	server2 := http.Server{
		Handler: mux2,
	}
	return &Proxy{serverForLocal: &server1, serverForRemote: &server2}
}

func (proxy *Proxy) StartServer() {
	cert, _ := tls.LoadX509KeyPair(CERTCRTFILE, CERTKEYFILE)
	caPem, _ := os.ReadFile(ROOTCAFILE)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPem)
	proxy.serverForLocal.TLSConfig = &tls.Config{
		ClientCAs:    pool,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	l1, _ := net.Listen("tcp", ":"+os.Getenv("PORT1"))
	go proxy.serverForLocal.ServeTLS(l1, "", "")

	proxy.serverForRemote.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	l2, _ := net.Listen("tcp", ":"+os.Getenv("PORT2"))
	go proxy.serverForRemote.ServeTLS(l2, "", "")
}
