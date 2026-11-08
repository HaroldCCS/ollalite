package main

// Estado en vivo del agente + latido: la interfaz siempre sabe qué está pasando
// (cargando el modelo, leyendo el prompt, generando sin texto visible, ejecutando una tool,
// esperando permiso) y se entera si Ollama o el modelo dejan de responder.

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Fases: prompt (Ollama lee el mensaje) · load (el modelo se está cargando en RAM) ·
// think / write (llegan tokens) · hidden (el modelo genera pero Ollama no muestra nada,
// típico de una llamada a tool larga) · tool (ejecutando) · approval (esperando permiso).
type Status struct {
	mu        sync.Mutex
	phase     string
	detail    string
	since     time.Time
	lastChunk time.Time
	tokens    int
	est       int
	round     int
	loaded    bool
	pending   *Event
}

func (s *Status) newRound(est int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.round++
	s.phase, s.detail, s.since = "prompt", "", time.Now()
	s.tokens, s.est, s.lastChunk, s.pending = 0, est, time.Time{}, nil
	s.loaded = true // hasta que el latido compruebe lo contrario
}

func (s *Status) set(phase, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phase || s.detail != detail {
		s.since = time.Now()
	}
	s.phase, s.detail, s.pending = phase, detail, nil
}

func (s *Status) waitApproval(call *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase, s.detail, s.since, s.pending = "approval", call.Name, time.Now(), call
}

func (s *Status) chunk(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.phase != phase {
		s.phase, s.since = phase, now
	}
	s.tokens++
	s.lastChunk = now
}

func (s *Status) setLoaded(v bool) {
	s.mu.Lock()
	s.loaded = v
	s.mu.Unlock()
}

func (s *Status) snapshot() Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, since := s.phase, s.since
	if (p == "think" || p == "write") && time.Since(s.lastChunk) > 2500*time.Millisecond {
		p, since = "hidden", s.lastChunk
	}
	if p == "prompt" && !s.loaded {
		p = "load"
	}
	e := Event{T: "st", P: p, D: s.detail, Ms: time.Since(since).Milliseconds(), N: s.tokens, Est: s.est}
	if p == "approval" && s.pending != nil {
		e.ID, e.Name = s.pending.ID, s.pending.Name
		if int(time.Since(since).Seconds())%10 == 0 { // los argumentos (pueden ser un archivo entero) cada ~10 s, no cada segundo
			e.Args = s.pending.Args
		}
	}
	return e
}

func (s *Status) phaseRound() (string, int) {
	e := s.snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.P, s.round
}

// heartbeat emite el estado cada segundo y vigila a Ollama mientras se espera al modelo.
// Si Ollama deja de responder o el modelo se cierra a mitad de respuesta, avisa y cancela.
func heartbeat(ctx context.Context, stop <-chan struct{}, model string, st *Status, emit func(Event), cancel context.CancelFunc) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	fails, gone, seenRound := 0, 0, -1
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		phase, round := st.phaseRound()
		if phase == "prompt" || phase == "load" || phase == "think" || phase == "write" || phase == "hidden" {
			loaded, err := modelLoaded(ctx, model)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if fails++; fails >= 3 {
					emit(Event{T: "err", D: "Ollama dejó de responder en " + ollamaBase.String() + ". Se canceló la respuesta; revisa que Ollama siga abierto."})
					cancel()
					return
				}
			default:
				fails = 0
				st.setLoaded(loaded)
				if loaded {
					seenRound, gone = round, 0
				} else if seenRound == round && phase != "prompt" && phase != "load" {
					if gone++; gone >= 2 { // estaba generando y el modelo desapareció de la memoria
						emit(Event{T: "err", D: "El modelo " + model + " se cerró mientras respondía (posible falta de RAM o un fallo de Ollama). Se canceló la respuesta."})
						cancel()
						return
					}
				}
			}
		}
		emit(st.snapshot())
	}
}

func modelLoaded(ctx context.Context, model string) (bool, error) {
	var r struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := ollamaJSON(cctx, "GET", "/api/ps", nil, &r); err != nil {
		return false, err
	}
	want := model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range r.Models {
		if m.Name == want || m.Model == want || m.Name == model {
			return true, nil
		}
	}
	return false, nil
}
