// Package service implements the kardianos service.Interface for
// ses-smtp-proxy and bundles the inbound and outbound workers behind a
// single Start/Stop lifecycle.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/kardianos/service"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/awsclient"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/inbound"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/outbound"
)

// Name is the service name as registered with the OS service manager. It
// also doubles as the Windows Event Log source.
const Name = "ses-smtp-proxy"

// DisplayName is shown in the Windows Services console and similar UIs.
const DisplayName = "SES to SMTP proxy"

// Description appears in the Windows Services console properties.
const Description = "Bridges Amazon SES and the on-premise Exchange SMTP server."

// NewServiceConfig returns the kardianos service configuration used for both
// install and run. exePath, when non-empty, overrides the binary path
// recorded in the service registration (useful when running from a build
// directory).
func NewServiceConfig() *service.Config {
	return &service.Config{
		Name:        Name,
		DisplayName: DisplayName,
		Description: Description,
		Arguments:   []string{"run"},
		Option: service.KeyValue{
			"StartType":              "automatic",
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "10s",
			"OnFailureResetPeriod":   60,
		},
	}
}

// Program implements service.Interface. It owns the long-lived context for
// the worker goroutines and the WaitGroup used to drain them on Stop.
type Program struct {
	Config *config.Config

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopErr chan error
}

// Start is called by kardianos right after the service starts. It must
// return promptly; long-running work happens in goroutines.
func (p *Program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.stopErr = make(chan error, 2)
	p.mu.Unlock()

	clients, err := awsclient.New(ctx, p.Config.AWS)
	if err != nil {
		cancel()
		return fmt.Errorf("init aws clients: %w", err)
	}

	inboundWorker := inbound.New(p.Config.Inbound, clients, slog.Default())
	outboundServer, err := outbound.New(p.Config.Outbound, clients, slog.Default())
	if err != nil {
		cancel()
		return fmt.Errorf("init outbound: %w", err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		err := inboundWorker.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("inbound worker stopped with error", "err", err)
		}
		p.stopErr <- err
	}()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		err := outboundServer.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("outbound server stopped with error", "err", err)
		}
		p.stopErr <- err
	}()

	slog.Info("ses-smtp-proxy started",
		"configPath", p.Config.Path,
		"servicePlatform", s.Platform(),
	)
	return nil
}

// Stop is called by kardianos when the service is being stopped.
func (p *Program) Stop(_ service.Service) error {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	slog.Info("ses-smtp-proxy stopped")
	return nil
}
