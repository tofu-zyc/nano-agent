package outbox

// breakJournal 让之后的写盘失败：模拟运行中磁盘出错。
func breakJournal(o *Outbox) { o.failWrite.Store(true) }
