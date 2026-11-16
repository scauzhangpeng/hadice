package mcpserver

// logcat 日志流的单元测试（不依赖真机/adb：只测解析、环形缓冲、游标语义与 URI）。

import (
	"testing"
)

func TestParseLogcatLine(t *testing.T) {
	e := parseLogcatLine("11-13 00:27:33.123  1234  1235 D FooBar : hello world")
	if e.Time != "11-13 00:27:33.123" || e.Pid != 1234 || e.Tid != 1235 {
		t.Fatalf("字段解析错误: %+v", e)
	}
	if e.Level != "D" || e.Tag != "FooBar" || e.Message != "hello world" {
		t.Fatalf("级别/Tag/消息解析错误: %+v", e)
	}

	// 分隔行：按原文保留。
	sep := parseLogcatLine("--------- beginning of main")
	if sep.Tag != "" || sep.Message != "--------- beginning of main" {
		t.Fatalf("分隔行应保留原文: %+v", sep)
	}
}

func TestLogCollectorRingAndCursor(t *testing.T) {
	c := newLogCollector("dev-1")
	c.capacity = 5 // 缩小容量便于验证溢出

	for i := 0; i < 8; i++ {
		c.append(LogLine{Message: "m" + string(rune('0'+i))})
	}

	// 环形缓冲只留最后 5 条，seq 单调。
	snap := c.tail(100)
	if len(snap.Entries) != 5 {
		t.Fatalf("环形缓冲长度 = %d, want 5", len(snap.Entries))
	}
	if snap.Entries[0].Seq != 4 || snap.Entries[4].Seq != 8 {
		t.Fatalf("seq 不符合预期: 首=%d 末=%d, want 4/8", snap.Entries[0].Seq, snap.Entries[4].Seq)
	}

	// since(6) 应返回 7、8 两条。
	inc := c.since(6)
	if len(inc.Entries) != 2 || inc.Entries[0].Seq != 7 || inc.Entries[1].Seq != 8 {
		t.Fatalf("增量错误: %+v", inc.Entries)
	}
	if inc.Truncated {
		t.Fatalf("cursor=6 未溢出，不应标记 truncated")
	}

	// since(0) 视为首次读，走 since 语义拿缓冲内全部。
	first := c.since(0)
	if len(first.Entries) != 5 {
		t.Fatalf("首次增量 = %d 条, want 5", len(first.Entries))
	}
}

func TestLogTruncationFlag(t *testing.T) {
	c := newLogCollector("dev-1")
	c.capacity = 5
	for i := 0; i < 8; i++ {
		c.append(LogLine{Message: "m"})
	}
	// cursor=1 时 2~3 已被丢弃 → truncated。
	inc := c.since(1)
	if !inc.Truncated {
		t.Fatalf("cursor=1 应标记 truncated")
	}
	// cursor >= oldest-1 时无丢弃。
	inc = c.since(3)
	if inc.Truncated {
		t.Fatalf("cursor=3 恰好接上最旧记录，不应 truncated")
	}
}

func TestParseLogURI(t *testing.T) {
	key, cursor, err := parseLogURI("hadice://device/emulator-5554/logcat/tail")
	if err != nil || key != "emulator-5554" || cursor != 0 {
		t.Fatalf("tail URI 解析错误: key=%q cursor=%d err=%v", key, cursor, err)
	}

	key, cursor, err = parseLogURI("hadice://device/192.168.1.5%3A5555/logcat/since/42")
	if err != nil || key != "192.168.1.5:5555" || cursor != 42 {
		t.Fatalf("since URI 解析错误: key=%q cursor=%d err=%v", key, cursor, err)
	}

	if _, _, err := parseLogURI("hadice://devices"); err == nil {
		t.Fatalf("非日志 URI 不应解析成功")
	}
	if _, _, err := parseLogURI("hadice://device/x/logcat/since/abc"); err == nil {
		t.Fatalf("非数字游标不应解析成功")
	}
}
