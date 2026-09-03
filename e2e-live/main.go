// Package main is the live E2E suite for the extension servers.
//
// It drives the DEPLOYED services over real NATS (abep protocol) and real
// jjlab/postgres — no mocks, no inproc. Run after a deployment:
//
//	go run .
//
// It exercises every tool of repo-extension and memory-extension end-to-end
// (envelope → tool.call.* → handler → result). See README for prerequisites.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/abcp-sdk/abc-protocol-go/agent"
	"github.com/abcp-sdk/abc-protocol-go/bus"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Target stack. Defaults point at the main zergx namespace; override with
// ABC_E2E_* env vars to run against another deployment (e.g. the zergx-dev
// copy in temp).
var (
	natsURL  = envOr("ABC_E2E_NATS", "nats://nats.temp.svc.cluster.local:4222")
	pgDSN    = envOr("ABC_E2E_PG", "postgres://root:devpassword@postgres.temp.svc.cluster.local:5432/zergx_agent")
	jjBase   = envOr("ABC_E2E_JJ", "http://jj-lab.temp.svc.cluster.local/api/v1")
	opsBase  = envOr("ABC_E2E_OPS", "http://ops-extension.temp.svc.cluster.local/api/v1")
	agentURL = envOr("ABC_E2E_AGENT", "http://agent.temp.svc.cluster.local")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var passed, failed int

func content(res agent.ToolResult) string {
	return res.Content
}

func check(name string, ok bool, detail string) {
	if ok {
		passed++
		fmt.Printf("  PASS: %s\n", name)
	} else {
		failed++
		fmt.Printf("  FAIL: %s  (%s)\n", name, detail)
	}
}

func main() {
	bus, err := natsbus.Connect(natsURL)
	if err != nil {
		fmt.Println("nats connect:", err)
		os.Exit(1)
	}
	defer bus.Close()
	ag := agent.New(bus)
	ctx := context.Background()

	call := func(session, ext, tool string, args map[string]interface{}) (agent.ToolResult, error) {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return ag.CallTool(c, session, ext, tool, "e2e-"+tool, args)
	}
	// callLong is for heavy tools (image builds) whose synchronous await
	// exceeds the default tool timeout.
	callLong := func(session, ext, tool string, args map[string]interface{}) (agent.ToolResult, error) {
		c, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		return ag.CallTool(c, session, ext, tool, "e2e-"+tool, args)
	}

	only := os.Getenv("ONLY")
	if only == "" || only == "memory" {
		runMemory(ctx, call)
	}
	if only == "" || only == "repo" {
		runRepo(ctx, call)
	}
	if only == "" || only == "ops" {
		runOps(ctx, call, callLong)
	}
	if only == "" || only == "lifecycle" {
		runLifecycle(ctx, call)
	}
	if only == "" || only == "progress" {
		runProgressInterrupt(ctx, ag, call, callLong)
	}
	if only == "" || only == "offload" {
		runOffload(ctx, ag, call)
	}
	if only == "" || only == "idem" {
		runIdempotency(ctx, bus)
	}

	fmt.Printf("\nRESULT: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// runLifecycle drives session lifecycle through the agent HTTP API and
// asserts repo-extension's workspace mirroring: created anchors a bookmark,
// forked inherits it, renamed dual-moves it, deleted removes it. The agent
// publishes lifecycle on abc.session.lifecycle.* after each HTTP commit;
// the extension processes asynchronously, so assertions poll with retries.
func runLifecycle(ctx context.Context, call func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== session lifecycle (agent HTTP -> repo-extension workspace) ===")

	agentBase := agentURL
	org := fmt.Sprintf("e2elf%d", time.Now().UnixNano()%1_000_000)
	post := func(path, body string) (int, string) {
		req, err := httpNew("POST", agentBase+path, body)
		if err != nil {
			return 0, err.Error()
		}
		resp, err := httpDo(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	branches := func(branch string) string {
		r, err := call("", "repo", "git-branches", map[string]interface{}{
			"_org": org, "_repo": "api", "_branch": branch,
		})
		if err != nil {
			return ""
		}
		return content(r)
	}
	waitBranches := func(want, notWant string) string {
		for i := 0; i < 10; i++ {
			got := branches("main")
			hasWant := want == "" || strings.Contains(got, want)
			hasNot := notWant != "" && strings.Contains(got, notWant)
			if hasWant && !hasNot {
				return got
			}
			time.Sleep(time.Second)
		}
		return branches("main")
	}

	s1 := org + ":api:main"
	code, body := post("/api/v1/sessions", `{"name":"`+s1+`"}`)
	check("lifecycle.create session", code == 200, fmt.Sprintf("code=%d body=%s", code, body))
	got := waitBranches("main", "")
	check("lifecycle.created anchors main bookmark", strings.Contains(got, "main"), fmt.Sprintf("branches=%q", got))

	fork := org + ":api:dev"
	code, body = post("/api/v1/sessions/"+s1+"/fork", `{"name":"`+fork+`"}`)
	check("lifecycle.fork session", code == 200, fmt.Sprintf("code=%d body=%s", code, body))
	got = waitBranches("dev", "")
	check("lifecycle.forked inherits bookmark", strings.Contains(got, "main") && strings.Contains(got, "dev"), fmt.Sprintf("branches=%q", got))

	renamed := org + ":api:feat"
	code, body = post("/api/v1/sessions/"+fork+"/rename", `{"name":"`+renamed+`"}`)
	check("lifecycle.rename session", code == 200, fmt.Sprintf("code=%d body=%s", code, body))
	got = waitBranches("feat", "dev")
	check("lifecycle.renamed dual-moves bookmark", strings.Contains(got, "feat") && !strings.Contains(got, "dev"), fmt.Sprintf("branches=%q", got))

	req, _ := httpNew("DELETE", agentBase+"/api/v1/sessions/"+renamed, "")
	resp, err := httpDo(req)
	dbody := ""
	if err == nil {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		dbody = string(b)
		code = resp.StatusCode
	} else {
		code = 0
	}
	check("lifecycle.delete session", code == 200, fmt.Sprintf("code=%d body=%s err=%v", code, dbody, err))
	got = waitBranches("", "feat")
	check("lifecycle.deleted removes bookmark", !strings.Contains(got, "feat") && strings.Contains(got, "main"), fmt.Sprintf("branches=%q", got))

	// cleanup: session first (the reconciler resurrects workspaces for
	// surviving sessions), then the org shells.
	for _, sid := range []string{s1} {
		req, _ := httpNew("DELETE", agentBase+"/api/v1/sessions/"+sid, "")
		if r, err := httpDo(req); err == nil {
			r.Body.Close()
		}
	}
	cleanupWorkspace("", org)
}

func runMemory(ctx context.Context, call func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== memory-extension (id=memory) ===")

	// todowrite
	r, err := call("e2e:smoke:main", "memory", "todowrite", map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "live e2e", "status": "pending", "priority": "high"},
		},
	})
	check("memory.todowrite", err == nil && strings.Contains(content(r), "1"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// history_search / history_range need a session with messages; seed PG directly.
	db, err := sql.Open("pgx", pgDSN)
	if err != nil {
		check("pg-connect", false, err.Error())
		return
	}
	defer db.Close()
	sid := "e2e-hist-" + fmt.Sprint(time.Now().UnixNano())
	m1 := "m1-" + sid
	m2 := "m2-" + sid
	_, _ = db.Exec(`INSERT INTO sessions (name, model, preset, created_at, updated_at) VALUES ($1,'','',now()::text,now()::text) ON CONFLICT (name) DO NOTHING`, sid)
	_, _ = db.Exec(`INSERT INTO messages (id, role, prev_id, created_at) VALUES ($1,'user','',now()::text),($2,'assistant','',now()::text)`, m1, m2)
	_, _ = db.Exec(`UPDATE messages SET prev_id=$1 WHERE id=$2`, m1, m2)
	_, _ = db.Exec(`UPDATE sessions SET tip_id=$1 WHERE name=$2`, m2, sid)
	_, _ = db.Exec(`INSERT INTO parts (id, message_id, type, seq, data) VALUES ($1,$2,'text',0,$3),($4,$5,'text',0,$6)`,
		"p1-"+sid, m1, `{"text":"hello from e2e keyword needle"}`, "p2-"+sid, m2, `{"text":"second message"}`)

	r, err = call(sid, "memory", "history_search", map[string]interface{}{"query": "needle", "limit": 10})
	check("memory.history_search", err == nil && strings.Contains(content(r), "1"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "memory", "history_range", map[string]interface{}{"from": 0, "to": 1, "limit": 10})
	check("memory.history_range", err == nil && strings.Contains(content(r), "1"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	_, _ = db.Exec(`DELETE FROM sessions WHERE name=$1`, sid)
	_, _ = db.Exec(`DELETE FROM todos WHERE session_id='e2e:smoke:main'`)
}

func runRepo(ctx context.Context, call func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== repo-extension (id=repo) ===")
	o, rp, bm := "build", "example", "main"

	// ensure test repo
	post := func(path string, body string) {
		req, _ := httpNew("POST", jjBase+path, body)
		resp, err := httpDo(req)
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
		_ = resp
	}
	post("/repos/ensure-org", `{"org":"`+o+`"}`)
	post("/repos/ensure", `{"org":"`+o+`","repo":"`+rp+`"}`)
	// no session bootstrap here on purpose: repo writes lazily adopt the
	// workspace, and the end-of-section cleanup deletes it again — a
	// persistent session would make the reconciler resurrect it forever.

	// write
	r, err := call("", "repo", "write", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "content": "line one\nline two\nline three\n", "message": "e2e write",
	})
	check("repo.write", err == nil && strings.Contains(content(r), "wrote file"), fmt.Sprintf("err=%v content=%q", err, content(r)))
	meta := r.Data.(map[string]interface{})
	cidWrite, _ := meta["change_id"].(string)

	// read
	r, err = call("", "repo", "read", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt",
	})
	check("repo.read", err == nil && strings.Contains(content(r), "1: line one") && strings.Contains(content(r), "3: line three"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// ls
	r, err = call("", "repo", "ls", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.ls", err == nil && strings.Contains(content(r), "e2e.txt"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// grep
	r, err = call("", "repo", "grep", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "pattern": "line two",
	})
	check("repo.grep", err == nil && strings.Contains(content(r), "line two"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// edit
	r, err = call("", "repo", "edit", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "start_line": 2, "end_line": 2, "content": "line TWO edited\n", "message": "e2e edit",
	})
	check("repo.edit", err == nil && strings.Contains(content(r), "edited file"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// read-after-edit
	r, err = call("", "repo", "read", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt",
	})
	check("repo.read-after-edit", err == nil && strings.Contains(content(r), "2: line TWO edited"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// git-log
	r, err = call("", "repo", "git-log", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.git-log", err == nil && strings.Contains(content(r), "commit"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// git-branches
	r, err = call("", "repo", "git-branches", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.git-branches", err == nil && strings.Contains(content(r), "main"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// explore
	r, err = call("", "repo", "explore", map[string]interface{}{})
	check("repo.explore", err == nil && strings.Contains(content(r), o), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// git-show: rev = write change_id
	r, err = call("", "repo", "git-show", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev": cidWrite,
	})
	check("repo.git-show", err == nil && strings.Contains(content(r), "e2e"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// second write → second commit for diff
	r, err = call("", "repo", "write", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e-v2.txt", "content": "v2\n", "message": "e2e v2",
	})
	meta2 := r.Data.(map[string]interface{})
	cidV2, _ := meta2["change_id"].(string)
	check("repo.write-2", err == nil && cidV2 != "", fmt.Sprintf("err=%v", err))

	// git-diff between the two changes
	r, err = call("", "repo", "git-diff", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev_a": cidWrite, "rev_b": cidV2, "path": "e2e-v2.txt",
	})
	check("repo.git-diff", err == nil, fmt.Sprintf("err=%v content=%q", err, content(r)))

	// git-blame
	r, err = call("", "repo", "git-blame", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev": bm, "path": "e2e-v2.txt",
	})
	check("repo.git-blame", err == nil && strings.Contains(content(r), "v2"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// delete
	r, err = call("", "repo", "delete", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "message": "e2e delete",
	})
	check("repo.delete", err == nil && strings.Contains(content(r), "deleted file"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// cleanup repo
	del := func(path string) {
		req, _ := httpNew("DELETE", jjBase+path, "")
		resp, err := httpDo(req)
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
	}
	del("/repos/" + o + "/" + rp)
	del("/repos/" + o)
}

func runOps(ctx context.Context, call func(string, string, string, map[string]interface{}) (agent.ToolResult, error), callLong func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== ops-extension (id=ops) ===")
	// Ops tools resolve the workspace from the session name
	// (org:repo:bookmark). Establish the test/dbg1 repo + main bookmark first
	// (the first sandbox-* tool call also lazily create the worker pod).
	sid := "test:dbg1:main"

	// ---- setup: ensure test/dbg1 repo + main bookmark exist ----
	{
		post := func(path string, body string) {
			req, _ := httpNew("POST", jjBase+path, body)
			if resp, err := httpDo(req); err == nil && resp != nil {
				resp.Body.Close()
			}
		}
		post("/repos/ensure-org", `{"org":"test"}`)
		post("/repos/ensure", `{"org":"test","repo":"dbg1"}`)
		// Seed a file so the repo has a real main bookmark head.
		_, _ = call("", "repo", "write", map[string]interface{}{
			"_org": "test", "_repo": "dbg1", "_branch": "main",
			"path": "e2e-ops-seed.txt", "content": "seed\n", "message": "e2e ops seed",
		})
	}

	// ---- sandbox lifecycle: write → read → edit → read ----
	// sandbox-* now require an explicit sandbox-create(image) first.
	r, err := call(sid, "ops", "sandbox-create", map[string]interface{}{
		"image": "docker.io/library/alpine:3.20",
	})
	check("ops.sandbox-create", err == nil && strings.Contains(content(r), "Created"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "sandbox-write", map[string]interface{}{
		"path": "e2e-ops.txt", "content": "line1\nline2\n",
	})
	check("ops.sandbox-write", err == nil && strings.Contains(content(r), "Wrote"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "sandbox-read", map[string]interface{}{"path": "e2e-ops.txt"})
	check("ops.sandbox-read", err == nil && strings.Contains(content(r), "line1") && strings.Contains(content(r), "line2"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "sandbox-edit", map[string]interface{}{
		"path": "e2e-ops.txt", "start_line": 1, "end_line": 1, "content": "line1-EDITED",
	})
	check("ops.sandbox-edit", err == nil && strings.Contains(content(r), "Edited"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "sandbox-read", map[string]interface{}{"path": "e2e-ops.txt"})
	check("ops.sandbox-read-after-edit", err == nil && strings.Contains(content(r), "line1-EDITED") && !strings.Contains(content(r), "\nline1\n"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// ---- sandbox-run (streamed job) ----
	r, err = call(sid, "ops", "sandbox-run", map[string]interface{}{"command": "echo ops-e2e-hello"})
	check("ops.sandbox-run", err == nil && strings.Contains(content(r), "ops-e2e-hello"), fmt.Sprintf("err=%v content=%q", err, content(r)))
	meta, _ := r.Data.(map[string]interface{})
	jobID, _ := meta["job_id"].(string)

	// ---- sandbox-job-list ----
	r, err = call(sid, "ops", "sandbox-job-list", map[string]interface{}{})
	check("ops.sandbox-job-list", err == nil && strings.Contains(content(r), jobID), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// ---- sandbox-job-output (job from sandbox-run) ----
	r, err = call(sid, "ops", "sandbox-job-output", map[string]interface{}{"job_id": jobID})
	check("ops.sandbox-job-output", err == nil && strings.Contains(content(r), "ops-e2e-hello"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// ---- sandbox-port (sandbox file → repo) ----
	r, err = call(sid, "ops", "sandbox-write", map[string]interface{}{
		"path": "port-e2e.txt", "content": "ported-e2e-content\n",
	})
	check("ops.sandbox-write-port", err == nil, fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "sandbox-port", map[string]interface{}{
		"sandbox_path": "port-e2e.txt", "repo_path": "ported-e2e.txt", "message": "e2e port",
	})
	check("ops.sandbox-port", err == nil && strings.Contains(content(r), "Ported"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// ---- stateless tools ----
	r, err = call(sid, "ops", "packages-search", map[string]interface{}{})
	check("ops.packages-search", err == nil && r.Error == nil && strings.Contains(content(r), "abc-protocol-go"), fmt.Sprintf("err=%v in-band=%+v content=%q", err, r.Error, content(r)))

	r, err = call(sid, "ops", "container-search", map[string]interface{}{})
	check("ops.container-search", err == nil && r.Error == nil && strings.Contains(content(r), "zergx-agent"), fmt.Sprintf("err=%v in-band=%+v content=%q", err, r.Error, content(r)))

	r, err = call(sid, "ops", "service-list", map[string]interface{}{})
	check("ops.service-list", err == nil && r.Error == nil, fmt.Sprintf("err=%v in-band=%+v content=%q", err, r.Error, content(r)))

	r, err = call(sid, "ops", "helm-list", map[string]interface{}{})
	check("ops.helm-list", err == nil && r.Error == nil, fmt.Sprintf("err=%v in-band=%+v", err, r.Error))

	// ---- heavy tools: container-build/deploy + helm lifecycle ----
	// Unique per-run names so reruns never collide; cleaned up at the end.
	runTag := fmt.Sprintf("e2e-ops-%d", time.Now().UnixNano())
	imgTag := "e2e-ops-img"
	releaseName := "e2e-ops-helm-reprobe"

	// Unique workspace per run via the supported bootstrap (session
	// lifecycle): the section's build tools resolve from this session.
	sorg := fmt.Sprintf("e2o%d", time.Now().UnixNano()%1_000_000)
	sid = sorg + ":smoke:main"
	if req, e := httpNew("POST", agentURL+"/api/v1/sessions", `{"name":"`+sid+`"}`); e == nil {
		if resp, e := httpDo(req); e == nil {
			resp.Body.Close()
		}
	}

	// container-build needs a Containerfile in the workspace repo. Write one
	// via repo-extension (test/dbg1), build it, then deploy from the image.
	seedOK := false
	for i := 0; i < 12; i++ {
		r, werr := call("", "repo", "write", map[string]interface{}{
			"_org": sorg, "_repo": "smoke", "_branch": "main",
			"path": "Containerfile", "content": "FROM library/busybox:1.36\nCOPY Containerfile /e2e-ops-marker.txt\nCMD [\"httpd\",\"-f\",\"-p\",\"8080\"]\n",
			"message": "e2e containerfile",
		})
		if werr == nil && strings.Contains(content(r), "wrote file") {
			seedOK = true
			break
		}
		time.Sleep(time.Second) // lifecycle bootstrap is asynchronous
	}
	check("ops.containerfile-seed", seedOK, "Containerfile never landed in "+sid)

	r, err = callLong(sid, "ops", "container-build", map[string]interface{}{
		"dockerfile_path": "Containerfile", "tag": imgTag,
	})
	check("ops.container-build", err == nil && strings.Contains(content(r), "Finished build"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// container-deploy: image must be the fully-qualified reference
	// ({registry}/{tag}:{bookmark}) so the deployment's pods can pull it.
	fullImage := envOr("ABC_E2E_REGISTRY", "jj-lab.temp.svc.cluster.local") + "/" + imgTag + ":main"
	r, err = call(sid, "ops", "container-deploy", map[string]interface{}{
		"image": fullImage, "name": runTag,
	})
	check("ops.container-deploy", err == nil && strings.Contains(content(r), "Deployed"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// helm-install needs a real chart in the workspace. Write a minimal chart,
	// install it, verify via status, then uninstall.
	chartFiles := map[string]string{
		"e2e-chart/Chart.yaml":               "apiVersion: v2\nname: e2e-ops-chart\nversion: 0.1.0\ndescription: e2e\n",
		"e2e-chart/values.yaml":              "replicaCount: 1\n",
		"e2e-chart/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: e2e-ops-probe-cm\ndata:\n  hello: world\n",
	}
	for p, c := range chartFiles {
		_, _ = call("", "repo", "write", map[string]interface{}{
			"_org": sorg, "_repo": "smoke", "_branch": "main", "path": p, "content": c, "message": "e2e chart",
		})
	}
	r, err = call(sid, "ops", "helm-install", map[string]interface{}{
		"release_name": releaseName, "chart_path": "e2e-chart",
	})
	check("ops.helm-install", err == nil && strings.Contains(content(r), "Finished helm"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "helm-status", map[string]interface{}{"release_name": releaseName})
	check("ops.helm-status", err == nil && strings.Contains(content(r), releaseName), fmt.Sprintf("err=%v content=%q", err, content(r)))

	r, err = call(sid, "ops", "helm-uninstall", map[string]interface{}{"release_name": releaseName})
	check("ops.helm-uninstall", err == nil && strings.Contains(content(r), "Uninstalled"), fmt.Sprintf("err=%v content=%q", err, content(r)))

	// cleanup: remove the deployed e2e deployment + service so reruns don't
	// leave ImagePullBackOff corpses in zergxd.
	cleanupDeploy := func(name string) {
		req, _ := httpNew("DELETE", opsBase+"/deployments/"+name, "")
		if resp, err := httpDo(req); err == nil && resp != nil {
			resp.Body.Close()
		}
	}
	cleanupDeploy(runTag)
	cleanupWorkspace(sid, sorg)
}

// runProgressInterrupt: during a real image build, progress telemetry must
// flow on abc.tool.progress.<callId>, and a session-scoped interrupt must
// abort the awaiting call well before the build ends.
func runProgressInterrupt(ctx context.Context, ag *agent.Agent, call, callLong func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== progress + interrupt (ops container-build) ===")

	// self-bootstrap a workspace for this build (lifecycle-anchored main)
	porg := fmt.Sprintf("e2p%d", time.Now().UnixNano()%1_000_000)
	psid := porg + ":prog:main"
	if req, e := httpNew("POST", agentURL+"/api/v1/sessions", `{"name":"`+psid+`"}`); e == nil {
		if resp, e := httpDo(req); e == nil {
			resp.Body.Close()
		}
	}
	seedOK := false
	for i := 0; i < 10; i++ {
		r, werr := call("", "repo", "write", map[string]interface{}{
			"_org": porg, "_repo": "prog", "_branch": "main",
			"path": "Containerfile", "content": "FROM library/busybox:1.36\nCOPY Containerfile /e2e-marker.txt\nCMD [\"httpd\",\"-f\",\"-p\",\"8080\"]\n", "message": "prog seed",
		})
		if werr == nil && strings.Contains(content(r), "wrote file") {
			seedOK = true
			break
		}
		time.Sleep(time.Second)
	}
	check("progress.workspace seeded", seedOK, "containerfile never landed")

	callID := "e2e-container-build" // must match the call helper ("e2e-"+tool)
	sub, err := ag.SubscribeProgress(ctx, callID)
	if err != nil {
		check("progress.subscribe", false, err.Error())
		return
	}
	got := make(chan string, 8)
	go func() {
		for i := 0; i < 8; i++ {
			env, ok := sub.Next(ctx)
			if !ok {
				return
			}
			b, _ := json.Marshal(env.Payload)
			got <- string(b)
		}
	}()

	done := make(chan agent.ToolResult, 1)
	errc := make(chan error, 1)
	go func() {
		tr, err := callLong(psid, "ops", "container-build", map[string]interface{}{
			"dockerfile_path": "Containerfile", "tag": "e2e-prog-img",
		})
		done <- tr
		errc <- err
	}()

	// wait for at least one progress event (build ~30s), then interrupt
	var firstProgress string
	select {
	case firstProgress = <-got:
		check("progress.telemetry received", strings.Contains(firstProgress, "running"), firstProgress)
	case <-time.After(60 * time.Second):
		check("progress.telemetry received", false, "no progress within 60s")
	}
	_ = sub.Close()

	if err := ag.InterruptSession(ctx, "ops", psid, "e2e interrupt test"); err != nil {
		check("interrupt.publish", false, err.Error())
	}
	select {
	case tr := <-done:
		err := <-errc // the goroutine always sends both
		interrupted := err == nil && tr.Error != nil && strings.Contains(tr.Error.Message, "interrupt")
		check("interrupt.aborts in-flight call", interrupted, fmt.Sprintf("err=%v res=%+v", err, tr.Error))
	case <-time.After(15 * time.Second):
		check("interrupt.aborts in-flight call", false, "call still pending 15s after interrupt")
	}
	cleanupWorkspace(psid, porg)
}

// runOffload: a >256KB tool result must land in the object store and the
// wire result carries an object ref (content stays a short head).
func runOffload(ctx context.Context, ag *agent.Agent, call func(string, string, string, map[string]interface{}) (agent.ToolResult, error)) {
	fmt.Println("=== object offload (repo big read) ===")

	big := strings.Repeat("OFFLOAD-MARKER\n", 30000) // ~420KB
	r, err := call("", "repo", "write", map[string]interface{}{
		"_org": "test", "_repo": "dbg1", "_branch": "main",
		"path": "big.txt", "content": big, "message": "e2e offload seed",
	})
	check("offload.seed write", err == nil && strings.Contains(content(r), "wrote file"), fmt.Sprintf("err=%v", err))

	r, err = call("", "repo", "read", map[string]interface{}{
		"_org": "test", "_repo": "dbg1", "_branch": "main", "path": "big.txt",
	})
	hasObj := err == nil && r.Object != nil
	check("offload.result carries object ref", hasObj, fmt.Sprintf("err=%v object=%+v", err, r.Object))
	if hasObj {
		objName := r.Object.Id
		data, gerr := ag.GetObject(ctx, objName)
		roundTrip := gerr == nil && data != nil && strings.Contains(string(data), "OFFLOAD-MARKER") && len(data) > 256*1024
		check("offload.object roundtrip", roundTrip, fmt.Sprintf("err=%v size=%d", gerr, len(data)))
	}
	if r.Content != "" {
		check("offload.content is a head", len(r.Content) <= 500, fmt.Sprintf("len=%d", len(r.Content)))
	}
}

// runIdempotency: the same durable-inbox publish id delivered twice inside
// the dedup window must be consumed exactly once (at-least-once + idempotent
// publish by id). Uses a dedicated session subject consumer so it never
// competes with the live agent's wildcard consumer.
func runIdempotency(ctx context.Context, nb bus.Bus) {
	fmt.Println("=== mailbox idempotency (duplicate publish id) ===")

	sid := fmt.Sprintf("e2e-idem:%d", time.Now().UnixNano())
	token := natsToken(sid)
	subj := "abc.mailbox." + token

	isub, err := nb.InboxConsume(ctx, bus.InboxConsumeOpts{Subject: subj})
	if err != nil {
		check("idempotency.subscribe", false, err.Error())
		return
	}
	defer func() { _ = isub.Close() }()

	time.Sleep(2 * time.Second) // let the pull consumer settle server-side
	id := fmt.Sprintf("e2e-dup-%d", time.Now().UnixNano())
	payload := map[string]interface{}{"id": id, "type": "e2e-idem", "payload": map[string]interface{}{"k": 1}}
	for i := 0; i < 2; i++ {
		if err := nb.InboxPublish(ctx, subj, payload, bus.InboxPublishOpts{ID: id, SessionName: sid}); err != nil {
			check("idempotency.publish", false, err.Error())
			return
		}
	}

	deliveries := 0
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			check("idempotency.exactly-once", deliveries == 1, fmt.Sprintf("deliveries=%d (want 1)", deliveries))
			return
		default:
		}
		cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
		msg, ok := isub.Next(cctx)
		ccancel()
		if !ok {
			continue
		}
		var inner struct {
			ID string `json:"id"`
		}
		b, _ := json.Marshal(msg.Envelope.Payload)
		_ = json.Unmarshal(b, &inner)
		if inner.ID == id {
			deliveries++
		}
		msg.Ack()
	}
}

// cleanupWorkspace removes a test session (agent PG) and its jjlab org
// shell. Session FIRST: the repo-extension reconciler resurrects workspaces
// for any surviving org:repo:bookmark session, so deleting the repo alone
// never sticks.
func cleanupWorkspace(sid, org string) {
	if req, e := httpNew("DELETE", agentURL+"/api/v1/sessions/"+sid, ""); e == nil {
		if resp, e := httpDo(req); e == nil {
			resp.Body.Close()
		}
	}
	if org == "" {
		return
	}
	// drop repos then the org shell
	if d, err := jsonGet(jjBase + "/repos?org=" + org); err == nil {
		var list struct {
			Orgs []struct {
				Repos []struct {
					Repo string `json:"repo"`
				} `json:"repos"`
			} `json:"orgs"`
		}
		if json.Unmarshal(d, &list) == nil {
			for _, o := range list.Orgs {
				for _, r := range o.Repos {
					if req, e := httpNew("DELETE", jjBase+"/repos/"+org+"/"+r.Repo, ""); e == nil {
						if resp, e := httpDo(req); e == nil {
							resp.Body.Close()
						}
					}
				}
			}
		}
	}
	if req, e := httpNew("DELETE", jjBase+"/repos/"+org, ""); e == nil {
		if resp, e := httpDo(req); e == nil {
			resp.Body.Close()
		}
	}
}

func jsonGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// natsToken mirrors the protocol's session token derivation.
func natsToken(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}
