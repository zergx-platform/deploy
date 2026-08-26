// Package main is the live E2E suite for the extension servers.
//
// It drives the DEPLOYED services over real NATS (abep protocol) and real
// jj-server/postgres — no mocks, no inproc. Run after a deployment:
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
	"os"
	"strings"
	"time"

	abep "abep.dev/sdk"
	natsbus "abep.dev/sdk/nats"
	"abep.dev/sdk/roleagent"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const natsURL = "nats://rucoder-nats.temp.svc.cluster.local:4222"
const pgDSN = "postgres://root:devpassword@rucoder-postgres.temp.svc.cluster.local:5432/rucoder_agent"
const jjBase = "http://rucoder-repo.temp.svc.cluster.local/api/v1"

var passed, failed int

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
	agent := abep.NewAgent(bus)
	ctx := context.Background()

	call := func(session, ext, tool string, args map[string]interface{}) (roleagent.ToolResult, error) {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return agent.CallTool(c, session, ext, tool, "e2e-"+tool, args, func(string) {})
	}

	runMemory(ctx, call)
	runRepo(ctx, call)
	runOps(ctx, call)

	fmt.Printf("\nRESULT: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func runMemory(ctx context.Context, call func(string, string, string, map[string]interface{}) (roleagent.ToolResult, error)) {
	fmt.Println("=== memory-extension (id=memory) ===")

	// todowrite
	r, err := call("e2e:smoke:main", "memory", "todowrite", map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "live e2e", "status": "pending", "priority": "high"},
		},
	})
	check("memory.todowrite", err == nil && strings.Contains(r.Content, "1"), fmt.Sprintf("err=%v content=%q", err, r.Content))

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
	check("memory.history_search", err == nil && strings.Contains(r.Content, "1"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "memory", "history_range", map[string]interface{}{"from": 0, "to": 1, "limit": 10})
	check("memory.history_range", err == nil && strings.Contains(r.Content, "1"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	_, _ = db.Exec(`DELETE FROM sessions WHERE name=$1`, sid)
	_, _ = db.Exec(`DELETE FROM todos WHERE session_id='e2e:smoke:main'`)
}

func runRepo(ctx context.Context, call func(string, string, string, map[string]interface{}) (roleagent.ToolResult, error)) {
	fmt.Println("=== repo-extension (id=repo) ===")
	o, rp, bm := "e2e", "smoke", "main"

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
	post("/repos/ensure-org", `{"org":"e2e"}`)
	post("/repos/ensure", `{"org":"e2e","repo":"smoke"}`)

	// write
	r, err := call("", "repo", "write", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "content": "line one\nline two\nline three\n", "message": "e2e write",
	})
	check("repo.write", err == nil && strings.Contains(r.Content, "已写入"), fmt.Sprintf("err=%v content=%q", err, r.Content))
	meta := r.Metadata.(map[string]interface{})
	cidWrite, _ := meta["change_id"].(string)

	// read
	r, err = call("", "repo", "read", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt",
	})
	check("repo.read", err == nil && strings.Contains(r.Content, "1: line one") && strings.Contains(r.Content, "3: line three"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ls
	r, err = call("", "repo", "ls", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.ls", err == nil && strings.Contains(r.Content, "e2e.txt"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// grep
	r, err = call("", "repo", "grep", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "pattern": "line two",
	})
	check("repo.grep", err == nil && strings.Contains(r.Content, "line two"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// edit
	r, err = call("", "repo", "edit", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "start_line": 2, "end_line": 2, "content": "line TWO edited\n", "message": "e2e edit",
	})
	check("repo.edit", err == nil && strings.Contains(r.Content, "已编辑"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// read-after-edit
	r, err = call("", "repo", "read", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt",
	})
	check("repo.read-after-edit", err == nil && strings.Contains(r.Content, "2: line TWO edited"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// git-log
	r, err = call("", "repo", "git-log", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.git-log", err == nil && strings.Contains(r.Content, "提交"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// git-branches
	r, err = call("", "repo", "git-branches", map[string]interface{}{"_org": o, "_repo": rp, "_branch": bm})
	check("repo.git-branches", err == nil && strings.Contains(r.Content, "main"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// explore
	r, err = call("", "repo", "explore", map[string]interface{}{})
	check("repo.explore", err == nil && strings.Contains(r.Content, "e2e"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// git-show: rev = write change_id
	r, err = call("", "repo", "git-show", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev": cidWrite,
	})
	check("repo.git-show", err == nil && strings.Contains(r.Content, "e2e"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// second write → second commit for diff
	r, err = call("", "repo", "write", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e-v2.txt", "content": "v2\n", "message": "e2e v2",
	})
	meta2 := r.Metadata.(map[string]interface{})
	cidV2, _ := meta2["change_id"].(string)
	check("repo.write-2", err == nil && cidV2 != "", fmt.Sprintf("err=%v", err))

	// git-diff between the two changes
	r, err = call("", "repo", "git-diff", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev_a": cidWrite, "rev_b": cidV2, "path": "e2e-v2.txt",
	})
	check("repo.git-diff", err == nil, fmt.Sprintf("err=%v content=%q", err, r.Content))

	// git-blame
	r, err = call("", "repo", "git-blame", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "rev": bm, "path": "e2e-v2.txt",
	})
	check("repo.git-blame", err == nil && strings.Contains(r.Content, "v2"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// delete
	r, err = call("", "repo", "delete", map[string]interface{}{
		"_org": o, "_repo": rp, "_branch": bm, "path": "e2e.txt", "message": "e2e delete",
	})
	check("repo.delete", err == nil && strings.Contains(r.Content, "已删除"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// cleanup repo
	del := func(path string) {
		req, _ := httpNew("DELETE", jjBase+path, "")
		resp, err := httpDo(req)
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
	}
	del("/repos/e2e/smoke")
	del("/repos/e2e")
}

func runOps(ctx context.Context, call func(string, string, string, map[string]interface{}) (roleagent.ToolResult, error)) {
	fmt.Println("=== ops-extension (id=ops) ===")
	// Ops tools resolve the workspace from the session name
	// (org:repo:bookmark). Use the pre-existing test/dbg1 repo + its already
	// running sandbox (session test:dbg1:main).
	sid := "test:dbg1:main"

	// ---- sandbox lifecycle: write → read → edit → read ----
	r, err := call(sid, "ops", "sandbox-write", map[string]interface{}{
		"path": "e2e-ops.txt", "content": "line1\nline2\n",
	})
	check("ops.sandbox-write", err == nil && strings.Contains(r.Content, "Wrote"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "sandbox-read", map[string]interface{}{"path": "e2e-ops.txt"})
	check("ops.sandbox-read", err == nil && strings.Contains(r.Content, "line1") && strings.Contains(r.Content, "line2"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "sandbox-edit", map[string]interface{}{
		"path": "e2e-ops.txt", "start_line": 1, "end_line": 1, "content": "line1-EDITED",
	})
	check("ops.sandbox-edit", err == nil && strings.Contains(r.Content, "Edited"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "sandbox-read", map[string]interface{}{"path": "e2e-ops.txt"})
	check("ops.sandbox-read-after-edit", err == nil && strings.Contains(r.Content, "line1-EDITED") && !strings.Contains(r.Content, "\nline1\n"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ---- sandbox-run (streamed job) ----
	r, err = call(sid, "ops", "sandbox-run", map[string]interface{}{"command": "echo ops-e2e-hello"})
	check("ops.sandbox-run", err == nil && strings.Contains(r.Content, "ops-e2e-hello"), fmt.Sprintf("err=%v content=%q", err, r.Content))
	meta, _ := r.Metadata.(map[string]interface{})
	jobID, _ := meta["job_id"].(string)

	// ---- sandbox-job-list ----
	r, err = call(sid, "ops", "sandbox-job-list", map[string]interface{}{})
	check("ops.sandbox-job-list", err == nil && strings.Contains(r.Content, jobID), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ---- sandbox-job-output (job from sandbox-run) ----
	r, err = call(sid, "ops", "sandbox-job-output", map[string]interface{}{"job_id": jobID})
	check("ops.sandbox-job-output", err == nil && strings.Contains(r.Content, "ops-e2e-hello"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ---- sandbox-port (sandbox file → repo) ----
	r, err = call(sid, "ops", "sandbox-write", map[string]interface{}{
		"path": "port-e2e.txt", "content": "ported-e2e-content\n",
	})
	check("ops.sandbox-write-port", err == nil, fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "sandbox-port", map[string]interface{}{
		"sandbox_path": "port-e2e.txt", "repo_path": "ported-e2e.txt", "message": "e2e port",
	})
	check("ops.sandbox-port", err == nil && strings.Contains(r.Content, "Ported"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ---- stateless tools ----
	r, err = call(sid, "ops", "list-containerfile-templates", map[string]interface{}{})
	check("ops.list-containerfile-templates", err == nil && strings.Contains(r.Content, "cargo"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "list-registry-packages", map[string]interface{}{})
	check("ops.list-registry-packages", err == nil && strings.Contains(r.Content, "abep.dev/sdk"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "image-list", map[string]interface{}{})
	check("ops.image-list", err == nil && strings.Contains(r.Content, "rucoder-agent-ts"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	r, err = call(sid, "ops", "helm-list", map[string]interface{}{})
	check("ops.helm-list", err == nil && strings.Contains(r.Content, "rucoder"), fmt.Sprintf("err=%v content=%q", err, r.Content))
}
