package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func (s *Service) startCodexTicketHarvester(ctx context.Context) {
	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	if s.ticketStopped || s.ticketDone != nil || s.coreManager == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.ticketCancel, s.ticketDone = cancel, done
	go func() {
		defer close(done)
		helps.DefaultCodexTurnTickets.Run(ctx, func() *config.Config {
			s.cfgMu.RLock()
			defer s.cfgMu.RUnlock()
			return s.cfg.CloneForRuntime()
		}, s.coreManager.List)
	}()
}

func (s *Service) stopCodexTicketHarvester() {
	s.ticketMu.Lock()
	s.ticketStopped = true
	cancel, done := s.ticketCancel, s.ticketDone
	s.ticketMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
