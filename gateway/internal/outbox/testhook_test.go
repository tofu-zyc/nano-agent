package outbox

import "nano-agent/gateway/internal/journal"

// breakJournal 让之后的写盘失败：模拟运行中磁盘出错。
func breakJournal(o *Outbox) { o.failWrite.Store(true) }

func journalOpen(path string) (*journal.Journal, error) {
	return journal.Open(path, 1, journal.Limits{Record: recordLimit, File: fileLimit}, nil)
}
