package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	bonnie "github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/nats"
	clientnats "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/presence"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	server "github.com/nats-io/nats-server/v2/server"
	gnats "github.com/nats-io/nats.go"
)

type fake struct{}

func (fake) PromptResult(context.Context, string) (*kit.TurnResult, error) {
	return &kit.TurnResult{Response: "smoke complete"}, nil
}
func (fake) InjectSteer(string) {}
func (fake) Close() error       { return nil }

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "broker":
			if err := broker(os.Args[2], os.Args[3]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "worker":
			worker(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
			return
		}
	}
	if err := orchestrate(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func broker(store, readyFile string) error {
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: store})
	if err != nil {
		return err
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		return fmt.Errorf("broker not ready")
	}
	if err := os.WriteFile(readyFile, []byte(s.ClientURL()), 0600); err != nil {
		return err
	}
	fmt.Println("BROKER READY", s.ClientURL())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	<-ctx.Done()
	s.Shutdown()
	return nil
}

func worker(id, journal, url, readyFile string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer func() { _ = listener.Close() }()
	httpURL := "http://" + listener.Addr().String()
	nc, err := gnats.Connect(url)
	must(err)
	reg, err := presencenats.New(context.Background(), nc, presencenats.Config{Bucket: "smokeworkers", TTL: 3 * time.Second, Create: true})
	must(err)
	fmt.Printf("WORKER READY id=%s\n", id)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	a := bonnie.New(bonnie.WithListener(listener), bonnie.WithJournal(journal), bonnie.WithContextFiles(""), bonnie.WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) { return fake{}, nil }), bonnie.WithNATS(nats.Config{Conn: nc, RootSubject: "smoke", WorkerID: id, TargetedTasks: true, CreateStream: true}), bonnie.WithPresence(bonnie.PresenceConfig{Registry: reg, WorkerID: id, RefreshInterval: 500 * time.Millisecond, Endpoints: []presence.Endpoint{{Channel: "http", Address: httpURL + "/bonnie/v1/runs", Input: true, Delivery: true}, {Channel: "nats", Address: "smoke.worker." + id, Input: true, Delivery: true}}}))
	if err := os.WriteFile(readyFile, []byte(httpURL), 0600); err != nil {
		panic(err)
	}
	if err := a.Run(ctx); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func orchestrate() (retErr error) {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "bonnie-presence-smoke-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmp); retErr == nil && err != nil {
			retErr = err
		}
	}()
	exe := filepath.Join(tmp, "smoke")
	cmd := exec.Command("go", "build", "-o", exe, "./internal/presencesmoke")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	broker := exec.Command(exe, "broker", filepath.Join(tmp, "broker"), filepath.Join(tmp, "broker.url"))
	broker.Stdout = os.Stdout
	broker.Stderr = os.Stderr
	if err := broker.Start(); err != nil {
		return err
	}
	var workers []*exec.Cmd
	defer func() {
		for _, child := range append(workers, broker) {
			if child.Process == nil || child.ProcessState != nil {
				continue
			}
			if err := child.Process.Kill(); err != nil && !isDone(child) && retErr == nil {
				retErr = err
			}
			if err := child.Wait(); err != nil {
				if _, ok := err.(*exec.ExitError); !ok && retErr == nil {
					retErr = err
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	readyFile := filepath.Join(tmp, "broker.url")
	var url string
	for url == "" {
		if b, err := os.ReadFile(readyFile); err == nil {
			url = string(b)
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("broker readiness timeout: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	nc, err := gnats.Connect(url)
	if err != nil {
		return err
	}
	defer nc.Close()
	reg, err := presencenats.New(ctx, nc, presencenats.Config{Bucket: "smokeworkers", TTL: 3 * time.Second, Create: true})
	if err != nil {
		return err
	}
	workerURLs := map[string]string{}
	for _, id := range []string{"w1", "w2"} {
		c := exec.Command(exe, "worker", id, filepath.Join(tmp, id), url, filepath.Join(tmp, id+".http"))
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			return err
		}
		workers = append(workers, c)
	}
	var records []presence.Record
	for {
		records, err = reg.Discover(ctx, presence.Filter{})
		if err == nil && len(records) == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("expected two workers, found %d: %v", len(records), err)
		case <-time.After(time.Second):
		}
	}
	fmt.Println("PASS discovery: w1 and w2 registered")
	for _, id := range []string{"w1", "w2"} {
		b, err := os.ReadFile(filepath.Join(tmp, id+".http"))
		if err != nil {
			return fmt.Errorf("worker %s HTTP endpoint not ready: %w", id, err)
		}
		workerURLs[id] = string(b)
	}
	if err := httpPresenceSmoke(ctx, records, workerURLs); err != nil {
		return err
	}
	snap, changes, err := reg.Watch(ctx, presence.Filter{})
	if err != nil {
		return err
	}
	if len(snap) != 2 {
		return fmt.Errorf("watch snapshot has %d workers, want 2", len(snap))
	}
	events := make(chan presence.Event, 16)
	watchErr := make(chan error, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case event, ok := <-changes:
				if !ok {
					watchErr <- fmt.Errorf("watch channel closed")
					return
				}
				events <- event
			}
		}
	}()
	client, err := clientnats.New(nc, clientnats.Config{RootSubject: "smoke", TargetedTasks: true, CreateStream: true})
	if err != nil {
		return err
	}
	outcome := make(chan clientnats.Outcome, 4)
	consumeCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = client.Consume(consumeCtx, func(_ context.Context, o clientnats.Outcome) error { outcome <- o; return nil })
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err = client.Submit(ctx, clientnats.Task{TaskID: "shared", Text: "shared"}); err != nil {
		return err
	}
	if _, err = client.SubmitTo(ctx, "w2", clientnats.Task{TaskID: "targeted", Text: "targeted"}); err != nil {
		return err
	}
	got := map[string]clientnats.Outcome{}
	for len(got) < 2 {
		select {
		case o := <-outcome:
			got[o.TaskID] = o
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for outcomes: %#v", got)
		}
	}
	for _, id := range []string{"shared", "targeted"} {
		o, ok := got[id]
		if !ok || o.Error != "" || o.State != runtime.RunCompleted {
			return fmt.Errorf("task %s outcome invalid: %+v", id, o)
		}
	}
	if got["shared"].WorkerID != "w1" && got["shared"].WorkerID != "w2" {
		return fmt.Errorf("shared task worker invalid: %+v", got["shared"])
	}
	if got["targeted"].WorkerID != "w2" {
		return fmt.Errorf("targeted task worker=%q want w2", got["targeted"].WorkerID)
	}
	fmt.Printf("PASS task outcomes: shared worker=%s targeted worker=%s\n", got["shared"].WorkerID, got["targeted"].WorkerID)
	if err := workers[0].Process.Kill(); err != nil {
		return err
	}
	if err := workers[0].Wait(); err == nil {
		return fmt.Errorf("expected killed w1 Wait error")
	}
	if err := awaitDeletion(ctx, events, watchErr, "w1"); err != nil {
		return err
	}
	fmt.Println("PASS watch deletion: w1")
	if err := workers[1].Process.Signal(os.Interrupt); err != nil {
		return err
	}
	if err := workers[1].Wait(); err != nil {
		return fmt.Errorf("graceful w2 shutdown: %w", err)
	}
	if err := awaitDeletion(ctx, events, watchErr, "w2"); err != nil {
		return err
	}
	if err := verifyHTTPStopped(ctx, workerURLs["w2"]); err != nil {
		return err
	}
	fmt.Println("PASS graceful shutdown removal: w2")
	return nil
}

func httpPresenceSmoke(ctx context.Context, records []presence.Record, urls map[string]string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, rec := range records {
		id := rec.Identity.Worker
		var endpoint *presence.Endpoint
		for i := range rec.Endpoints {
			if rec.Endpoints[i].Channel == "http" {
				endpoint = &rec.Endpoints[i]
				break
			}
		}
		if endpoint == nil || endpoint.Address != urls[id]+"/bonnie/v1/runs" || !endpoint.Input || !endpoint.Delivery || !endpoint.Ready {
			return fmt.Errorf("worker %s HTTP presence endpoint invalid: %+v", id, endpoint)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.Address, bytes.NewBufferString(`{"text":"presence HTTP smoke"}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("POST worker %s HTTP endpoint: %w", id, err)
		}
		var body struct {
			RunID    string           `json:"run_id"`
			State    runtime.RunState `json:"state"`
			Response string           `json:"response"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		if err := resp.Body.Close(); err != nil {
			return err
		}
		if decodeErr != nil {
			return decodeErr
		}
		if resp.StatusCode != http.StatusOK || body.RunID == "" || body.State != runtime.RunCompleted || body.Response != "smoke complete" {
			return fmt.Errorf("worker %s HTTP response invalid: status=%d body=%+v", id, resp.StatusCode, body)
		}
	}
	fmt.Println("PASS HTTP presence: discovered endpoints accepted POSTs and completed runs")
	return nil
}

func awaitDeletion(ctx context.Context, events <-chan presence.Event, watchErr <-chan error, id string) error {
	for {
		select {
		case event := <-events:
			if event.Record.Identity.Worker == id && event.Deleted {
				return nil
			}
		case err := <-watchErr:
			return err
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s deletion: %w", id, ctx.Err())
		}
	}
}
func verifyHTTPStopped(ctx context.Context, endpoint string) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/bonnie/v1/health", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("PASS HTTP shutdown: graceful worker endpoint stopped accepting requests")
			return nil
		}
		if err := resp.Body.Close(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("HTTP endpoint still accepting requests after graceful exit: %s", endpoint)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func isDone(cmd *exec.Cmd) bool { return cmd.ProcessState != nil }
