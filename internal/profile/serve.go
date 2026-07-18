package profile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
)

type Server struct {
	Listener net.Listener
	HTTP     *http.Server
	URLs     []string
}

func NewServer(bundleDir, listen string) (*Server, error) {
	bundleDir, err := filepath.Abs(bundleDir)
	if err != nil {
		return nil, err
	}
	if _, err := readManifest(bundleDir); err != nil {
		return nil, err
	}
	if listen == "" {
		listen = ":8765"
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		listener.Close()
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes)
	prefix := "/" + token + "/"
	files := http.StripPrefix(prefix, http.FileServer(http.Dir(bundleDir)))
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, prefix) {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(writer, request)
	})
	server := &Server{Listener: listener, HTTP: &http.Server{Handler: handler}}
	port := listener.Addr().(*net.TCPAddr).Port
	server.URLs = serverURLs(port, token)
	return server, nil
}

func (server *Server) Serve() error {
	err := server.HTTP.Serve(server.Listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (server *Server) Close(ctx context.Context) error {
	return server.HTTP.Shutdown(ctx)
}

func serverURLs(port int, token string) []string {
	seen := map[string]bool{}
	var urls []string
	add := func(host string) {
		value := fmt.Sprintf("http://%s:%d/%s", host, port, token)
		if !seen[value] {
			seen[value] = true
			urls = append(urls, value)
		}
	}
	add("127.0.0.1")
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if item.Flags&net.FlagUp == 0 || item.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := item.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil {
				add(ip.String())
			}
		}
	}
	return urls
}
