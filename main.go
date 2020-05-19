// Copyright 2016 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"cloud.google.com/go/compute/metadata"
)

var jobs []*job

type job struct {
	ID   string
	From string
	To   string

	HTTPCookie string

	// Status reporting
	mu            sync.Mutex
	lastOK        time.Time // last healthy status
	statusTime    time.Time // time status was set
	statusOK      bool      // normal state?
	statusMessage string    // status indicator, suitable for public use
}

func main() {
	var (
		// repo URLs (legacy)
		from = os.Getenv("FROM_REPO")
		to   = os.Getenv("TO_REPO")

		// repo spec (json)
		spec = os.Getenv("REPOS")
	)
	if spec != "" {
		spec = reconcile(spec)
		if err := json.Unmarshal([]byte(spec), &jobs); err != nil {
			log.Fatalf("Could not parse REPOS: %v", err)
		}
	} else if from != "" && to != "" {
		jobs = append(jobs, &job{ID: "default", From: from, To: to})
	} else {
		log.Fatalf("REPOS environment variable must be set.")
	}

	for _, j := range jobs {
		if j.ID == "" {
			log.Fatalf("Missing ID for job %+v", j)
		}
		if j.From == "" || j.To == "" {
			log.Fatalf("Empty from or to for job %+v", j)
		}
		j.From = reconcile(j.From)
		j.To = reconcile(j.To)
		j.statusOK = true

		go j.mirror()
	}

	http.Handle("/", http.RedirectHandler("https://github.com/broady/reposync", http.StatusTemporaryRedirect))
	http.HandleFunc("/status", statusz)

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// reconcile gets a value from the GCE metadata server if the given string is
// prefixed with "metadata:".
func reconcile(s string) string {
	if !strings.HasPrefix(s, "metadata:") {
		return s
	}
	val, err := metadata.ProjectAttributeValue(s[len("metadata:"):])
	if err != nil {
		log.Fatalf("Could not get project metadata value %q: %v", s, err)
	}
	return val
}

func (j *job) dir() string {
	return "repo-" + j.ID
}

func (j *job) cookiefile() string {
	return "cookies-" + j.ID
}

func (j *job) mirror() {
	j.ok("Cloning")

	for {
		cmd := exec.Command("git", "clone", j.From, j.dir())
		out, err := cmd.CombinedOutput()
		if err == nil {
			j.ok("Cloned", out)
			break
		}
		j.statusErr("Cloning", err, out)
		os.RemoveAll(j.dir())
		time.Sleep(10 * time.Second)
		continue
	}

	if j.HTTPCookie != "" {
		if err := ioutil.WriteFile(j.cookiefile(), []byte(j.HTTPCookie), 0400); err != nil {
			j.statusErr("Writing HTTP cookie file", err)
		}
		cmd := exec.Command("git", "config", "http.cookiefile", j.cookiefile())
		cmd.Dir = j.dir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			j.statusErr("Set cookie file", err, out)
		} else {
			j.ok("Set http.cookiefile")
		}
	}

	for {
		j.ok("Setting remote")
		cmd := exec.Command("git", "remote", "add", "to", j.To)
		cmd.Dir = j.dir()
		out, err := cmd.CombinedOutput()
		if err == nil {
			j.ok("Added remote", out)
			break
		}
		j.statusErr("Adding remote", err, out)
		time.Sleep(time.Second)
	}

	limit := rate.NewLimiter(rate.Every(time.Minute), 1)

	var oldSHA, oldTags []byte

	for {
		ctx := context.Background()
		limit.Wait(ctx)

		j.logf("Pulling")
		cmd := exec.CommandContext(ctx, "git", "pull")
		cmd.Dir = j.dir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			j.statusErr("Pull", err, out)
			continue
		}
		j.logf("Pulled: %s", out)

		sha, err := ioutil.ReadFile(j.dir() + "/.git/refs/heads/master")
		if err != nil {
			j.statusErr("parse HEAD", err)
			continue
		}

		cmd = exec.CommandContext(ctx, "git", "tag", "-l")
		cmd.Dir = j.dir()
		tags, err := cmd.CombinedOutput()
		if err != nil {
			j.statusErr("git tag -l", tags)
			continue
		}

		if !bytes.Equal(sha, oldSHA) {
			j.logf("Pushing")
			cmd = exec.CommandContext(ctx, "git", "push", "--all", "to")
			cmd.Dir = j.dir()
			out, err = cmd.CombinedOutput()
			if err != nil {
				j.statusErr("Push", err, out)
				continue
			}
		}

		if !bytes.Equal(tags, oldTags) {
			j.logf("Pushing tags")
			cmd = exec.CommandContext(ctx, "git", "push", "--tags", "to")
			cmd.Dir = j.dir()
			out, err = cmd.CombinedOutput()
			if err != nil {
				j.statusErr("Push tags", err, out)
				continue
			}
		}

		j.ok("Synced")
		oldSHA = sha
		oldTags = tags
	}
}

func statusz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")

	for _, j := range jobs {
		j.mu.Lock()
		if time.Since(j.lastOK) > 15*time.Minute {
			w.WriteHeader(500)
			// Stale. Something went wrong.
			fmt.Fprintf(w, "Repo %q possibly not fresh\n", j.ID)
		}
		j.mu.Unlock()
	}

	for _, j := range jobs {
		j.mu.Lock()
		fmt.Fprintln(w, "---- repo", j.ID, "----")
		fmt.Fprintln(w, "OK now?   ", j.statusOK)
		fmt.Fprintln(w, "Last OK:  ", j.lastOK)
		fmt.Fprintln(w, "Last try: ", j.statusTime)
		fmt.Fprintln(w, j.statusMessage)
		j.mu.Unlock()
	}
}

func (j *job) logf(msg string, v ...interface{}) {
	out := fmt.Sprintf("["+j.ID+"] "+msg, v...)

	// Redact the from/to, just in case there are secrets in the URL (e.g., GitHub token)
	out = strings.ReplaceAll(out, j.From, "<REDACTED (FROM)>")
	out = strings.ReplaceAll(out, j.To, "<REDACTED (TO)>")

	log.Println(out)
}

func (j *job) ok(msg string, v ...interface{}) {
	j.status(true, msg, v...)
}

func (j *job) statusErr(msg string, v ...interface{}) {
	j.status(false, msg, v...)
}

func (j *job) status(ok bool, msg string, v ...interface{}) {
	j.mu.Lock()

	j.statusOK = ok
	j.statusMessage = msg
	j.statusTime = time.Now()
	if ok {
		j.lastOK = time.Now()
	}

	j.mu.Unlock()

	// Log potentially sensitive output.

	buf := &bytes.Buffer{}
	fmt.Fprintln(buf, msg)
	for _, vv := range v {
		switch vv.(type) {
		case []byte:
			fmt.Fprintf(buf, "%s\n", vv)
		default:
			fmt.Fprintf(buf, "%v\n", vv)
		}
	}

	if ok {
		j.logf("OK: %s", buf.String())
	} else {
		j.logf("FAIL: %s", buf.String())
	}
}
