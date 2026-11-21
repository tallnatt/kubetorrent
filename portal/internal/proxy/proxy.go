package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

type Router struct {
	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy
}

func NewRouter() *Router {
	return &Router{
		proxies: make(map[string]*httputil.ReverseProxy),
	}
}

func (rt *Router) ServeUser(w http.ResponseWriter, r *http.Request, backendURL string) {
	if backendURL == "" {
		http.Error(w, "no backend configured for user", http.StatusBadGateway)
		return
	}

	p, err := rt.getOrCreate(backendURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid backend URL: %v", err), http.StatusBadGateway)
		return
	}
	p.ServeHTTP(w, r)
}

func (rt *Router) getOrCreate(rawURL string) (*httputil.ReverseProxy, error) {
	rt.mu.RLock()
	p, ok := rt.proxies[rawURL]
	rt.mu.RUnlock()
	if ok {
		return p, nil
	}

	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
		// Rewrite Origin and Referer so qBittorrent's CSRF check matches the backend host
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", target.Scheme+"://"+target.Host)
		}
		if req.Header.Get("Referer") != "" {
			req.Header.Set("Referer", target.Scheme+"://"+target.Host+"/")
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "qBittorrent backend unavailable: "+err.Error(), http.StatusBadGateway)
	}

	rt.mu.Lock()
	rt.proxies[rawURL] = rp
	rt.mu.Unlock()
	return rp, nil
}
