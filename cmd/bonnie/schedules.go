package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newSchedulesCmd() *cobra.Command {
	c := &cobra.Command{Use: "schedules", Short: "Inspect and trigger schedules", RunE: func(*cobra.Command, []string) error { return fmt.Errorf("schedules needs a subcommand") }}
	c.AddCommand(scheduleRead("list", "List schedules", false), scheduleRead("show <name>", "Show schedule metadata and history", true), scheduleRead("history <name>", "Show schedule history", true), scheduleTrigger())
	return c
}
func scheduleRead(use, short string, named bool) *cobra.Command {
	var base, token string
	args := cobra.NoArgs
	if named {
		args = cobra.ExactArgs(1)
	}
	cmd := &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
		suffix := ""
		if named {
			suffix = "/" + url.PathEscape(args[0])
		}
		return scheduleRequest(cmd, base, token, http.MethodGet, suffix, nil, use == "history <name>")
	}}
	cmd.Flags().StringVar(&base, "url", "http://127.0.0.1:8080", "BONNIE server URL")
	cmd.Flags().StringVar(&token, "token", "", "bearer token")
	return cmd
}
func scheduleTrigger() *cobra.Command {
	var base, token, id, at, kind string
	cmd := &cobra.Command{Use: "trigger <name>", Short: "Trigger a schedule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		t := time.Now().UTC()
		if at != "" {
			parsed, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return fmt.Errorf("--scheduled-at must be RFC3339: %w", err)
			}
			t = parsed
		} else if kind != "manual" || id != "" {
			return fmt.Errorf("external triggers require --scheduled-at and --id")
		}
		return scheduleRequest(cmd, base, token, http.MethodPost, "/"+url.PathEscape(args[0])+"/trigger", map[string]any{"id": id, "scheduled_at": t, "kind": kind}, false)
	}}
	f := cmd.Flags()
	f.StringVar(&base, "url", "http://127.0.0.1:8080", "BONNIE server URL")
	f.StringVar(&token, "token", "", "bearer token")
	f.StringVar(&id, "id", "", "occurrence ID (empty generates one)")
	f.StringVar(&at, "scheduled-at", "", "scheduled time in RFC3339")
	f.StringVar(&kind, "kind", "manual", "trigger kind")
	return cmd
}
func scheduleRequest(cmd *cobra.Command, base, token, method, path string, body any, history bool) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(cmd.Context(), method, strings.TrimRight(base, "/")+"/bonnie/v1/schedules"+path, &b)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	w := cmd.OutOrStdout()
	if history {
		var envelope struct {
			History json.RawMessage `json:"history"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
			return err
		}
		if len(envelope.History) == 0 {
			return fmt.Errorf("server response has no history")
		}
		_, err = w.Write(append(envelope.History, '\n'))
		return err
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
	return err
}
