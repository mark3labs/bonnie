package bonnie

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/schedule"
)

const schedulesPath = "/bonnie/v1/schedules"

type scheduleService struct {
	engine *schedule.Engine
	lock   *os.File
	cancel context.CancelFunc
	done   chan error
}

func newScheduleService(c *config, runner *runtime.Runner, channels []Channel) (*scheduleService, error) {
	if len(c.schedules) == 0 {
		return nil, nil
	}
	destinations := make(map[string]channel.TrackedReceiver)
	for _, ch := range channels {
		if r, ok := ch.(channel.TrackedReceiver); ok {
			destinations[ch.Name()] = r
		}
	}
	e, err := schedule.New(runner, destinations, c.schedules...)
	if err != nil {
		return nil, fmt.Errorf("bonnie: schedules: %w", err)
	}
	if err := os.MkdirAll(c.journal, 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: schedule lock directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(c.journal, ".schedule.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("bonnie: schedule lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("bonnie: schedule journal is already owned: %w", err)
	}
	return &scheduleService{engine: e, lock: f}, nil
}

func (s *scheduleService) mount(mux *http.ServeMux, c *config) {
	if s == nil {
		return
	}
	mux.HandleFunc("GET "+schedulesPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeRead(w, r, c) {
			return
		}
		writeScheduleJSON(w, s.engine.List())
	})
	mux.HandleFunc("GET "+schedulesPath+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeRead(w, r, c) {
			return
		}
		name := r.PathValue("name")
		defs := s.engine.List()
		var d any
		for _, x := range defs {
			if x.Name == name {
				d = x
				break
			}
		}
		if d == nil {
			http.NotFound(w, r)
			return
		}
		h, err := s.engine.History(r.Context(), name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeScheduleJSON(w, map[string]any{"schedule": d, "history": h})
	})
	if c.scheduleAuthorizer != nil {
		mux.HandleFunc("POST "+schedulesPath+"/{name}/trigger", func(w http.ResponseWriter, r *http.Request) {
			if !s.authorizeRead(w, r, c) {
				return
			}
			if err := c.scheduleAuthorizer(r); err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var body struct {
				ID          string     `json:"id"`
				ScheduledAt *time.Time `json:"scheduled_at"`
				Kind        string     `json:"kind"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil {
				http.Error(w, "invalid request body", 400)
				return
			}
			if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
				http.Error(w, "invalid request body", 400)
				return
			}
			if body.Kind == "external" && (body.ScheduledAt == nil || body.ID == "") {
				http.Error(w, "external triggers require id and scheduled_at", 400)
				return
			}
			if body.ScheduledAt == nil {
				now := time.Now().UTC()
				body.ScheduledAt = &now
			}
			if body.ID == "" {
				var b [16]byte
				if _, err := rand.Read(b[:]); err != nil {
					http.Error(w, "cannot generate id", 500)
					return
				}
				body.ID = hex.EncodeToString(b[:])
			}
			kind := body.Kind
			if kind == "" {
				kind = "manual"
			}
			o, err := s.engine.Trigger(r.Context(), r.PathValue("name"), body.ID, *body.ScheduledAt, kind)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			writeScheduleJSON(w, o)
		})
	}
}
func (s *scheduleService) authorizeRead(w http.ResponseWriter, r *http.Request, c *config) bool {
	if c.auth == nil {
		return true
	}
	if _, err := c.auth(r); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, bonniehttp.ErrUnauthenticated) {
			status = http.StatusUnauthorized
		}
		http.Error(w, "authentication failed", status)
		return false
	}
	return true
}
func writeScheduleJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *scheduleService) start(parent context.Context, clock bool) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan error, 1)
	go func() {
		if clock {
			s.done <- s.engine.Start(ctx)
		} else {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					s.done <- ctx.Err()
					return
				case <-ticker.C:
					if err := s.engine.Reconcile(ctx); err != nil {
						s.done <- err
						return
					}
				}
			}
		}
	}()
}
func (s *scheduleService) shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	var err error
	if s.done != nil {
		select {
		case e := <-s.done:
			if e != nil && !errors.Is(e, context.Canceled) {
				err = e
			}
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if e := s.engine.Shutdown(ctx); e != nil {
		err = errors.Join(err, e)
	}
	if s.lock != nil {
		if e := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); e != nil {
			err = errors.Join(err, e)
		}
		if e := s.lock.Close(); e != nil {
			err = errors.Join(err, e)
		}
	}
	return err
}
