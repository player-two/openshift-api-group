package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const group = "openshift.org"

func main() {
	var (
		port       int
		token      string
		insecure   bool
		caCertPath string
	)
	flag.IntVar(&port, "p", 8080, "port number")
	flag.StringVar(&token, "t", "", "auth token")
	flag.BoolVar(&insecure, "insecure", false, "skip tls checks")
	flag.StringVar(&caCertPath, "cacert", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "cacert")
	flag.Parse()

	kubeHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	kubePort := os.Getenv("KUBERNETES_SERVICE_PORT")

	if kubeHost == "" {
		log.Fatal("kubernetes host not found in environment")
		return
	}

	if kubePort == "" {
		log.Fatal("kubernetes port not found in environment")
		return
	}

	proxyUrl, err := url.Parse(fmt.Sprintf("https://%s:%s", kubeHost, kubePort))
	if err != nil {
		log.Fatal(err)
		return
	}

	if token == "" {
		buf, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			log.Fatal(err)
			return
		}
		token = string(buf)
	}

	tlsConfig := &tls.Config{}
	if insecure {
		tlsConfig.InsecureSkipVerify = true
	} else {
		caCert, err := ioutil.ReadFile(caCertPath)
		if err != nil {
			log.Fatal(err)
			return
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		tlsConfig.RootCAs = caCertPool
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(proxyUrl)
	reverseProxy.Transport = &http.Transport{TLSClientConfig: tlsConfig}

	http.Handle(fmt.Sprintf("/apis/%s/", group), logHandler(authInjectionHandler(token, http.StripPrefix(fmt.Sprintf("/apis/%s", group), addPrefix("/oapi", groupTransformer(reverseProxy))))))
	http.Handle("/apis", logHandler(authInjectionHandler(token, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		res := httptest.NewRecorder()
		reverseProxy.ServeHTTP(res, req)

		if err := addOpenshiftAPIGroup(res.Body, w); err != nil {
			ErrorResponse(w, err)
		}
	}))))
	authedProxy := authInjectionHandler(token, reverseProxy)
	http.Handle("/healthz", authedProxy)
	http.Handle("/", logHandler(authedProxy))

	log.Printf("Proxying requests to %s:%s", kubeHost, kubePort)
	log.Printf("Listening on port %d", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func ErrorResponse(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "{\"error\":\"%+v\"}", err)
}

type loggingResponseWriter struct {
	writer     http.ResponseWriter
	statusCode int
}

func (lw *loggingResponseWriter) Header() http.Header {
	return lw.writer.Header()
}

func (lw *loggingResponseWriter) Write(b []byte) (int, error) {
	return lw.writer.Write(b)
}

func (lw *loggingResponseWriter) WriteHeader(statusCode int) {
	lw.statusCode = statusCode
	lw.writer.WriteHeader(statusCode)
}

func logHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		lw := &loggingResponseWriter{w, 200}
		next(lw, req)
		log.Printf("%s %s %d\n", req.Method, path, lw.statusCode)
	}
}

func logHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(logHandlerFunc(h.ServeHTTP))
}

func authInjectionHandler(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
		h.ServeHTTP(w, req)
	})
}

func changeGroup(group string, src io.Reader, dest io.Writer) error {
	var manifest map[string]interface{}
	if err := json.NewDecoder(src).Decode(&manifest); err != nil {
		return err
	}

	apiVersion, ok := manifest["apiVersion"].(string)
	if ok {
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return err
		}
		gv.Group = group
		manifest["apiVersion"] = gv.String()
	}

	return json.NewEncoder(dest).Encode(manifest)
}

func groupTransformer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		buf := new(bytes.Buffer)
		if req.Method != http.MethodGet && req.Method != http.MethodDelete {
			if err := changeGroup("", req.Body, buf); err != nil {
				ErrorResponse(w, err)
				return
			}
			req.ContentLength = int64(buf.Len())
			req.Body = ioutil.NopCloser(buf)
		}

		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)

		buf.Reset()
		if err := changeGroup(group, res.Body, buf); err != nil {
			ErrorResponse(w, err)
			return
		}

		for k, v := range res.HeaderMap {
			for _, h := range v {
				w.Header().Add(k, h)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(res.Code)
		io.Copy(w, buf)
	})
}

func addPrefix(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = prefix + req.URL.Path
		h.ServeHTTP(w, req)
	})
}

func addOpenshiftAPIGroup(input io.Reader, output io.Writer) error {
	var groupList metav1.APIGroupList
	if err := json.NewDecoder(input).Decode(&groupList); err != nil {
		return err
	}
	openshiftVersion := metav1.GroupVersionForDiscovery{
		GroupVersion: fmt.Sprintf("%s/v1", group),
		Version:      "v1",
	}
	openshiftGroup := metav1.APIGroup{
		Name:             group,
		Versions:         []metav1.GroupVersionForDiscovery{openshiftVersion},
		PreferredVersion: openshiftVersion,
	}
	groupList.Groups = append(groupList.Groups, openshiftGroup)
	return json.NewEncoder(output).Encode(groupList)
}
