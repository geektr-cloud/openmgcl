package cmd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
)

// checkPublishSafety enforces the non-loopback exposure gate before the
// encryption key is loaded, the database is opened, or a listener is created.
// Only the literal host 127.0.0.1 is exempt for both the listen host and an
// explicitly configured server URL.
func checkPublishSafety(host, serverURL string, skip bool, stderr io.Writer) error {
	listenHost, err := hostOnlyFromListenAddress(host)
	if err != nil {
		return fmt.Errorf("invalid listen host %q: %w", host, err)
	}

	hosts := []string{listenHost}
	serverHost := ""
	if serverURL != "" {
		serverHost, err = hostOnlyFromServerURL(serverURL)
		if err != nil {
			return fmt.Errorf("invalid server URL: %w", err)
		}
		hosts = append(hosts, serverHost)
	}

	if listenHost == "127.0.0.1" && (serverURL == "" || serverHost == "127.0.0.1") {
		return nil
	}

	fmt.Fprintf(stderr, "WARNING: MgCl will expose state and secrets outside the loopback interface on %s.\n",
		strings.Join(hosts, " and "))
	fmt.Fprintln(stderr, "Network traffic may be unencrypted, and other hosts may be able to read state, secret values and credentials.")

	if skip {
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("refusing non-loopback listen without confirmation; pass --yes-i-know-it-will-publish-state-and-secrets")
	}

	fmt.Fprint(stderr, "Continue? [y/N] ")
	answer, err := readTerminalLine()
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("non-loopback listen was not confirmed")
	}
}

func hostOnlyFromListenAddress(hostport string) (string, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	return host, nil
}

func hostOnlyFromServerURL(raw string) (string, error) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			// Do not wrap url.Error: it includes the full URL, which may carry
			// userinfo or a token in its query string.
			return "", errors.New("invalid URL")
		}
		if u.Host == "" {
			return "", errors.New("missing host")
		}
		host := u.Hostname()
		if host == "" {
			return "", errors.New("missing host")
		}
		return host, nil
	}

	value := strings.TrimRight(raw, "/")
	if value == "" {
		return "", errors.New("missing host")
	}
	if strings.ContainsAny(value, "?#/@ \t\r\n") {
		return "", errors.New("invalid host")
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host, nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	if net.ParseIP(value) != nil {
		return value, nil
	}
	if host, zone, ok := strings.Cut(value, "%"); ok && zone != "" && net.ParseIP(host) != nil {
		return value, nil
	}
	if strings.ContainsAny(value, ":%") {
		return "", errors.New("invalid host")
	}
	return value, nil
}
