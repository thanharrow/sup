package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// Client is a wrapper over the SSH connection/sessions.
type SSHClient struct {
	conn           *ssh.Client
	sess           *ssh.Session
	user           string
	host           string
	remoteStdin    io.WriteCloser
	remoteStdout   io.Reader
	remoteStderr   io.Reader
	connOpened     bool
	sessOpened     bool
	running        bool
	env            string //export FOO="bar"; export BAR="baz";
	color          string
	privateKeyFile string
}

type ErrConnect struct {
	User   string
	Host   string
	Reason string
}

func (e ErrConnect) Error() string {
	return fmt.Sprintf(`Connect("%v@%v"): %v`, e.User, e.Host, e.Reason)
}

// parseHost parses and normalizes <user>@<host:port> from a given string.
func (c *SSHClient) parseHost(host string) error {
	c.host = host

	// Remove extra "ssh://" schema
	if len(c.host) > 6 && c.host[:6] == "ssh://" {
		c.host = c.host[6:]
	}

	// Split by the last "@", since there may be an "@" in the username.
	if at := strings.LastIndex(c.host, "@"); at != -1 {
		c.user = c.host[:at]
		c.host = c.host[at+1:]
	}

	// Add default user, if not set
	if c.user == "" {
		u, err := user.Current()
		if err != nil {
			return err
		}
		c.user = u.Username
	}

	if strings.Contains(c.host, "/") {
		return ErrConnect{c.user, c.host, "unexpected slash in the host URL"}
	}

	// Add default port, if not set
	if !strings.Contains(c.host, ":") {
		c.host += ":22"
	}

	return nil
}

var initAuthMethodOnce sync.Once
var authMethod ssh.AuthMethod

// parsePrivateKey reads and parses an SSH private key from file.
func parsePrivateKey(file string) (ssh.Signer, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		if strings.Contains(err.Error(), "passphrase") || strings.Contains(err.Error(), "decipher") || strings.Contains(err.Error(), "encrypted") {
			fd := int(os.Stdin.Fd())
			var tty *os.File
			if !term.IsTerminal(fd) {
				if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
					tty = f
					defer tty.Close()
					fd = int(tty.Fd())
				}
			}

			out := os.Stdout
			if tty != nil {
				out = tty
			}
			fmt.Fprintf(out, "Enter passphrase for '%s': ", filepath.Base(file))

			passphrase, err := term.ReadPassword(fd)
			if err != nil {
				fmt.Fprintln(out)
				return nil, err
			}
			fmt.Fprintln(out)

			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, passphrase)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return signer, nil
}

// parsePublicKeyFile reads and parses an SSH public key from file.
func parsePublicKeyFile(file string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, err
	}
	return pubKey, nil
}

// findSignerInAgent searches the running SSH Agent for a signer matching the specified key path.
func findSignerInAgent(privateKeyPath string) (ssh.Signer, error) {
	pubKeyPath := privateKeyPath + ".pub"
	pubKey, err := parsePublicKeyFile(pubKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read/parse public key file %s: %v", pubKeyPath, err)
	}

	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set")
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSH agent: %v", err)
	}

	agentClient := agent.NewClient(conn)
	signers, err := agentClient.Signers()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get signers from agent: %v", err)
	}

	targetBytes := pubKey.Marshal()
	for _, s := range signers {
		if bytes.Equal(s.PublicKey().Marshal(), targetBytes) {
			return s, nil
		}
	}

	conn.Close()
	return nil, fmt.Errorf("key not found in SSH agent")
}

// initAuthMethod initiates SSH authentication method.
func initAuthMethod() {
	var signers []ssh.Signer

	// If there's a running SSH Agent, try to use its Private keys.
	sock, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err == nil {
		// Note: Do NOT defer sock.Close() here. The returned agent.Signers() proxy
		// signers need to communicate with the SSH agent via this socket during
		// the SSH handshake later.
		agent := agent.NewClient(sock)
		signers, _ = agent.Signers()
	}

	// Try to read user's SSH private keys form the standard paths.
	files, _ := filepath.Glob(os.Getenv("HOME") + "/.ssh/id_*")
	for _, file := range files {
		if strings.HasSuffix(file, ".pub") {
			continue // Skip public keys.
		}
		signer, err := parsePrivateKey(file)
		if err != nil {
			continue
		}
		signers = append(signers, signer)
	}
	authMethod = ssh.PublicKeys(signers...)
}

// SSHDialFunc can dial an ssh server and return a client
type SSHDialFunc func(net, addr string, config *ssh.ClientConfig) (*ssh.Client, error)

// Connect creates SSH connection to a specified host.
// It expects the host of the form "[ssh://]host[:port]".
func (c *SSHClient) Connect(host string) error {
	return c.ConnectWith(host, ssh.Dial)
}

// ConnectWith creates a SSH connection to a specified host. It will use dialer to establish the
// connection.
// TODO: Split Signers to its own method.
func (c *SSHClient) ConnectWith(host string, dialer SSHDialFunc) error {
	if c.connOpened {
		return fmt.Errorf("Already connected")
	}

	err := c.parseHost(host)
	if err != nil {
		return err
	}

	var auths []ssh.AuthMethod

	if c.privateKeyFile != "" {
		signer, err := parsePrivateKey(c.privateKeyFile)
		if err != nil {
			// If parsing fails (e.g. unhandled hardware key), attempt to find the matching signer in the SSH agent.
			agentSigner, agentErr := findSignerInAgent(c.privateKeyFile)
			if agentErr != nil {
				return ErrConnect{
					User:   c.user,
					Host:   c.host,
					Reason: fmt.Sprintf("private key file: %v\nNote: For FIDO/Yubikey security keys (sk-*), you must add the key to your SSH agent first:\n  ssh-add %s", err, c.privateKeyFile),
				}
			}
			signer = agentSigner
		}
		auths = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	} else {
		initAuthMethodOnce.Do(initAuthMethod)
		auths = []ssh.AuthMethod{authMethod}
	}

	config := &ssh.ClientConfig{
		User: c.user,
		Auth: auths,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
	}

	c.conn, err = dialer("tcp", c.host, config)
	if err != nil {
		reason := err.Error()
		if strings.Contains(reason, "agent: failed to sign challenge") && c.privateKeyFile != "" {
			reason = fmt.Sprintf(`%s
			Note: This signature failure typically occurs for 
			FIDO2/Yubikey security keys (sk-*) requiring a PIN or touch confirmation 
			if the SSH agent cannot prompt for user input. 

			To fix this:
			1. Ensure you have ssh-askpass installed (e.g. "brew install theseal/ssh-askpass/ssh-askpass" on macOS).
			2. Configure your environment, start the agent, and add the key:
			   export SSH_ASKPASS_REQUIRE=force
			   export SSH_ASKPASS=/opt/homebrew/bin/ssh-askpass
			   eval $(ssh-agent -s)
			   ssh-add %s
			3. Alternatively, use an SSH key without a PIN (touch-only) or a standard SSH key.`, reason, c.privateKeyFile)
		}
		return ErrConnect{c.user, c.host, reason}
	}
	c.connOpened = true

	return nil
}

// Run runs the task.Run command remotely on c.host.
func (c *SSHClient) Run(task *Task) error {
	if c.running {
		return fmt.Errorf("Session already running")
	}
	if c.sessOpened {
		return fmt.Errorf("Session already connected")
	}

	sess, err := c.conn.NewSession()
	if err != nil {
		return err
	}

	c.remoteStdin, err = sess.StdinPipe()
	if err != nil {
		return err
	}

	c.remoteStdout, err = sess.StdoutPipe()
	if err != nil {
		return err
	}

	c.remoteStderr, err = sess.StderrPipe()
	if err != nil {
		return err
	}

	if task.TTY {
		// Set up terminal modes
		modes := ssh.TerminalModes{
			ssh.ECHO:          0,     // disable echoing
			ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
			ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
		}
		// Request pseudo terminal
		if err := sess.RequestPty("xterm", 80, 40, modes); err != nil {
			return ErrTask{task, fmt.Sprintf("request for pseudo terminal failed: %s", err)}
		}
	}

	// Start the remote command.
	if err := sess.Start(c.env + task.Run); err != nil {
		return ErrTask{task, err.Error()}
	}

	c.sess = sess
	c.sessOpened = true
	c.running = true
	return nil
}

// Wait waits until the remote command finishes and exits.
// It closes the SSH session.
func (c *SSHClient) Wait() error {
	if !c.running {
		return fmt.Errorf("Trying to wait on stopped session")
	}

	err := c.sess.Wait()
	c.sess.Close()
	c.running = false
	c.sessOpened = false

	return err
}

// DialThrough will create a new connection from the ssh server sc is connected to. DialThrough is an SSHDialer.
func (sc *SSHClient) DialThrough(net, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := sc.conn.Dial(net, addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil

}

// Close closes the underlying SSH connection and session.
func (c *SSHClient) Close() error {
	if c.sessOpened {
		c.sess.Close()
		c.sessOpened = false
	}
	if !c.connOpened {
		return fmt.Errorf("Trying to close the already closed connection")
	}

	err := c.conn.Close()
	c.connOpened = false
	c.running = false

	return err
}

func (c *SSHClient) Stdin() io.WriteCloser {
	return c.remoteStdin
}

func (c *SSHClient) Stderr() io.Reader {
	return c.remoteStderr
}

func (c *SSHClient) Stdout() io.Reader {
	return c.remoteStdout
}

func (c *SSHClient) Prefix() (string, int) {
	host := c.user + "@" + c.host + " | "
	return c.color + host + ResetColor, len(host)
}

func (c *SSHClient) Write(p []byte) (n int, err error) {
	return c.remoteStdin.Write(p)
}

func (c *SSHClient) WriteClose() error {
	return c.remoteStdin.Close()
}

func (c *SSHClient) Signal(sig os.Signal) error {
	if !c.sessOpened {
		return fmt.Errorf("session is not open")
	}

	switch sig {
	case os.Interrupt:
		// TODO: Turns out that .Signal(ssh.SIGHUP) doesn't work for me.
		// Instead, sending \x03 to the remote session works for me,
		// which sounds like something that should be fixed/resolved
		// upstream in the golang.org/x/crypto/ssh pkg.
		// https://github.com/golang/go/issues/4115#issuecomment-66070418
		c.remoteStdin.Write([]byte("\x03"))
		return c.sess.Signal(ssh.SIGINT)
	default:
		return fmt.Errorf("%v not supported", sig)
	}
}
