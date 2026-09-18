package agent

// spawnhook_test.go - spawn 钩子（会话登记表拿到引擎子进程 pid 的唯一途径）。
//
// 为什么关键：引擎 CLI 自成进程组（Setpgid），调用方杀 magic-agent 的进程组带不走它。
// 只有把**子进程 pid** 记进会话登记表，`magic-agent --stop <会话>` 才能停对进程。
// 这里用真引擎（ClaudeEngine / CodexEngine）+ 假 CLI 验证钩子确实收到子进程 pid。

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSpawnHookReceivesChildPID(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-1","model":"m"}'
`)
	e := &ClaudeEngine{BinPath: cli}

	type seen struct {
		pid int
	}
	got := make(chan seen, 4)
	ctx := WithSpawnHook(context.Background(), func(pid int) { got <- seen{pid} })

	if _, err := e.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	select {
	case s := <-got:
		if s.pid <= 0 {
			t.Fatalf("钩子收到的 pid 非法: %d", s.pid)
		}
		if s.pid == os.Getpid() {
			t.Error("钩子应给**子进程** pid，不是 magic-agent 自己")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("spawn 钩子没被调用 —— --stop 会杀错进程")
	}
}

// 流式路径同样要触发钩子（观物台的对话走的是 --stream）。
func TestSpawnHookFiresOnStreamPath(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-2","model":"m"}'
`)
	e := &ClaudeEngine{BinPath: cli}

	got := make(chan int, 4)
	ctx := WithSpawnHook(context.Background(), func(pid int) { got <- pid })

	if _, err := e.Stream(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case pid := <-got:
		if pid <= 0 || pid == os.Getpid() {
			t.Errorf("流式路径的钩子 pid 不对: %d", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("流式路径没触发 spawn 钩子")
	}
}

// 没挂钩子时不能崩（WithSpawnHook(nil) / 空 ctx 都要安全）。
func TestSpawnHookAbsentIsSafe(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-3","model":"m"}'
`)
	e := &ClaudeEngine{BinPath: cli}

	// nil 回调 → 原样返回 ctx
	if ctx := WithSpawnHook(context.Background(), nil); ctx == nil {
		t.Fatal("WithSpawnHook(nil) 不该返回 nil ctx")
	}
	// 没挂钩子的普通调用照常工作
	if _, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("无钩子调用失败: %v", err)
	}
	// 直接调 notifySpawn 也不能崩
	notifySpawn(context.Background(), 12345)
	notifySpawn(nil, 1)
}

// PIDAlive：当前进程活着、明显不存在的 pid 不活着。
func TestPIDAlive(t *testing.T) {
	if !PIDAlive(os.Getpid()) {
		t.Error("自身 pid 应判定为活着")
	}
	if PIDAlive(0) || PIDAlive(-1) {
		t.Error("非法 pid 应判定为不活着")
	}
}
