package redeo

import (
	"bufio"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Server configuration
type Server struct {
	config   *Config
	info     *ServerInfo
	commands map[string]Handler

	tcp, unix net.Listener
	clients   *clients
}

// NewServer creates a new server instance
func NewServer(config *Config) *Server {
	if config == nil {
		config = DefaultConfig
	}

	clients := newClientRegistry()
	return &Server{
		config:   config,
		clients:  clients,
		info:     newServerInfo(config, clients),
		commands: make(map[string]Handler),
	}
}

// Addr returns the server TCP address
func (srv *Server) Addr() string {
	return srv.config.Addr
}

// Socket returns the server UNIX socket address
func (srv *Server) Socket() string {
	return srv.config.Socket
}

// Info returns the server info registry
func (srv *Server) Info() *ServerInfo {
	return srv.info
}

// Close shuts down the server and closes all connections
func (srv *Server) Close() (err error) {

	// Stop new TCP connections
	if srv.tcp != nil {
		if e := srv.tcp.Close(); e != nil {
			err = e
		}
		srv.tcp = nil
	}

	// Stop new Unix socket connections
	if srv.unix != nil {
		if e := srv.unix.Close(); e != nil {
			err = e
		}
		srv.unix = nil
	}

	// Terminate all clients
	if e := srv.clients.Clear(); err != nil {
		err = e
	}

	return
}

// Handle registers a handler for a command.
// Not thread-safe, don't call from multiple goroutines
func (srv *Server) Handle(name string, handler Handler) {
	srv.commands[strings.ToLower(name)] = handler
}

// HandleFunc registers a handler callback for a command
func (srv *Server) HandleFunc(name string, callback HandlerFunc) {
	srv.Handle(name, Handler(callback))
}

// Apply applies a request
func (srv *Server) Apply(req *Request) (*Responder, error) {
	cmd, ok := srv.commands[req.Name]
	if !ok {
		return nil, UnknownCommand(req.Name)
	}

	srv.info.onCommand()
	if req.client != nil {
		req.client.trackCommand(req.Name)
	}
	res := NewResponder()
	err := cmd.ServeClient(res, req)
	return res, err
}

// ListenAndServe starts the server
func (srv *Server) ListenAndServe() (err error) {
	errs := make(chan error, 2)

	if srv.Addr() != "" {
		srv.tcp, err = net.Listen("tcp", srv.Addr())
		if err != nil {
			return
		}
		go srv.serve(errs, srv.tcp)
	}

	if srv.Socket() != "" {
		srv.unix, err = srv.listenUnix()
		if err != nil {
			return err
		}
		go srv.serve(errs, srv.unix)
	}

	return <-errs
}

// accepts incoming connections on the Listener lis, creating a
// new service goroutine for each.
func (srv *Server) serve(errs chan error, lis net.Listener) {
	defer lis.Close()

	for {
		conn, err := lis.Accept()
		if err != nil {
			errs <- err
			return
		}
		go srv.serveClient(NewClient(conn))
	}
}

// Starts a new session, serving client
func (srv *Server) serveClient(client *Client) {
	// Register client
	srv.clients.Put(client)
	defer srv.clients.Close(client.id)

	// Track connection
	srv.info.onConnect()

	// Apply TCP keep-alive, if configured
	if alive := srv.config.TCPKeepAlive; alive > 0 {
		if tcpconn, ok := client.conn.(*net.TCPConn); ok {
			tcpconn.SetKeepAlive(true)
			tcpconn.SetKeepAlivePeriod(alive)
		}
	}

	// Init request/response loop
	buffer := bufio.NewReader(client.conn)
	for {
		if timeout := srv.config.Timeout; timeout > 0 {
			client.conn.SetDeadline(time.Now().Add(timeout))
		}

		req, err := ParseRequest(buffer)
		if err != nil {
			srv.writeError(client.conn, err)
			return
		}
		req.client = client

		res, err := srv.Apply(req)
		if err != nil {
			srv.writeError(client.conn, err)
			// Don't disconnect clients on simple command errors to allow pipelining
			if _, ok := err.(ClientError); ok {
				continue
			}
			return
		}

		if _, err = res.WriteTo(client.conn); err != nil {
			return
		} else if client.quit {
			return
		}
	}
}

// Serve starts a new session, using `conn` as a transport.
func (srv *Server) writeError(conn net.Conn, err error) {
	// Don't try to respond on EOFs
	if err == io.EOF {
		return
	}
	res := NewResponder()
	res.WriteError(err)
	res.WriteTo(conn)
}

// listenUnix starts the unix listener on socket path
func (srv *Server) listenUnix() (net.Listener, error) {
	if stat, err := os.Stat(srv.Socket()); !os.IsNotExist(err) && !stat.IsDir() {
		if err = os.RemoveAll(srv.Socket()); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", srv.Socket())
}
