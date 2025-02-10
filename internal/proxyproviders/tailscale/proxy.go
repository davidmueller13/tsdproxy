// SPDX-FileCopyrightText: 2025 Paulo Almeida <almeidapaulopt@gmail.com>
// SPDX-License-Identifier: MIT

package tailscale

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/almeidapaulopt/tsdproxy/internal/model"
	"github.com/almeidapaulopt/tsdproxy/internal/proxyproviders"

	"github.com/rs/zerolog"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// Proxy struct implements proxyconfig.Proxy.
type Proxy struct {
	log      zerolog.Logger
	config   *model.Config
	tsServer *tsnet.Server
	lc       *tailscale.LocalClient
	ctx      context.Context

	events chan proxyproviders.ProxyEvent

	authURL string
	url     string
	status  model.ProxyStatus

	mtx sync.Mutex
}

var (
	_ proxyproviders.ProxyInterface = (*Proxy)(nil)

	ErrProxyPortNotFound = errors.New("proxy port not found")
)

// Start method implements proxyconfig.Proxy Start method.
func (p *Proxy) Start(ctx context.Context) error {
	var (
		err error
		lc  *tailscale.LocalClient
	)

	if err = p.tsServer.Start(); err != nil {
		return err
	}

	if lc, err = p.tsServer.LocalClient(); err != nil {
		return err
	}

	p.mtx.Lock()
	p.ctx = ctx
	p.lc = lc
	p.mtx.Unlock()

	go p.watchStatus()

	return nil
}

func (p *Proxy) GetURL() string {
	// TODO: should be configurable and not force to https
	return "https://" + p.url
}

// Close method implements proxyconfig.Proxy Close method.
func (p *Proxy) Close() error {
	if p.tsServer != nil {
		return p.tsServer.Close()
	}

	return nil
}

func (p *Proxy) GetListener(port string) (net.Listener, error) {
	portCfg, ok := p.config.Ports[port]
	if !ok {
		return nil, ErrProxyPortNotFound
	}

	network := portCfg.ProxyProtocol
	if portCfg.ProxyProtocol == "http" || portCfg.ProxyProtocol == "https" {
		network = "tcp"
	}
	addr := ":" + strconv.Itoa(portCfg.ProxyPort)

	if portCfg.Tailscale.Funnel {
		return p.tsServer.ListenFunnel(network, addr)
	}
	if portCfg.ProxyProtocol == "https" {
		return p.tsServer.ListenTLS(network, addr)
	}
	return p.tsServer.Listen(network, addr)
}

func (p *Proxy) WatchEvents() chan proxyproviders.ProxyEvent {
	return p.events
}

func (p *Proxy) GetAuthURL() string {
	return p.authURL
}

func (p *Proxy) watchStatus() {
	watcher, err := p.lc.WatchIPNBus(p.ctx, ipn.NotifyInitialState|ipn.NotifyNoPrivateKeys|ipn.NotifyInitialHealthState)
	if err != nil {
		p.log.Error().Err(err).Msg("tailscale.watchStatus")
		return
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				p.log.Error().Err(err).Msg("tailscale.watchStatus: Next")
			}
			return
		}

		if n.ErrMessage != nil {
			p.log.Error().Str("error", *n.ErrMessage).Msg("tailscale.watchStatus: backend")
			return
		}

		status, err := p.lc.Status(p.ctx)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			p.log.Error().Err(err).Msg("tailscale.watchStatus: status")
			return
		}

		switch status.BackendState {
		case "NeedsLogin":
			if status.AuthURL != "" {
				p.setStatus(model.ProxyStatusAuthenticating, "", status.AuthURL)
			}
		case "Starting":
			p.setStatus(model.ProxyStatusStarting, "", "")
		case "Running":
			p.setStatus(model.ProxyStatusRunning, strings.TrimRight(status.Self.DNSName, "."), "")
			if p.status != model.ProxyStatusRunning {
				p.getTLSCertificates()
			}
		}
	}
}

func (p *Proxy) setStatus(status model.ProxyStatus, url string, authURL string) {
	if p.status == status && p.url == url && p.authURL == authURL {
		return
	}

	p.log.Debug().Str("authURL", url).Str("status", status.String()).Msg("tailscale status")

	p.mtx.Lock()
	p.status = status
	if url != "" {
		p.url = url
	}
	if authURL != "" {
		p.authURL = authURL
	}
	p.mtx.Unlock()

	p.events <- proxyproviders.ProxyEvent{
		Status: status,
	}
}

func (p *Proxy) getTLSCertificates() {
	p.log.Info().Msg("Generating TLS certificate")
	certDomains := p.tsServer.CertDomains()
	if _, _, err := p.lc.CertPair(p.ctx, certDomains[0]); err != nil {
		p.log.Error().Err(err).Msg("error to get TLS certificates")
		return
	}
	p.log.Info().Msg("TLS certificate generated")
}
