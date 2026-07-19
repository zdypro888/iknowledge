package engine

import (
	"errors"
	"fmt"
	"sort"

	"github.com/zdypro888/iknowledge/internal/store"
)

// prepareTruthTransactionLocked 把所有将修改的 .knowledge 相对路径写入仓外 WAL。
// 前提：已持 e.rt.mu 写锁，且此后首个 truth write 发生前不能再新增目标路径。
func (e *Engine) prepareTruthTransactionLocked(rels map[string]bool) (*store.TruthTransaction, error) {
	if e.rt.truthTxActive {
		return nil, fmt.Errorf("engine: 已有活跃 truth transaction")
	}
	ordered := make([]string, 0, len(rels))
	for rel := range rels {
		if rel != "" {
			ordered = append(ordered, rel)
		}
	}
	sort.Strings(ordered)
	tx, err := e.Store.PrepareTruthTransaction(ordered)
	if err != nil {
		return nil, err
	}
	e.rt.truthTxActive = true
	return tx, nil
}

// guardTruthTransactionPanicLocked 必须在 prepare 成功后立刻 defer。HTTP server
// 会 recover handler panic，panic 不等于进程退出；若不在同一进程先回滚并清 active，
// 后续 reload 会永久跳过 prepared WAL，继续暴露半应用真相。
func (e *Engine) guardTruthTransactionPanicLocked(tx *store.TruthTransaction) {
	panicValue := recover()
	if panicValue == nil {
		return
	}
	if recoveryErr := e.rollbackTruthTransactionLocked(tx, nil); recoveryErr != nil {
		panic(fmt.Errorf("truth transaction panic (%v); recovery failed: %w", panicValue, recoveryErr))
	}
	panic(panicValue)
}

// rollbackTruthTransactionLocked 恢复 before-image，并在 WAL 清理后重载缓存。
// 即使本进程回滚失败，也先解除 active 标记再让 reload 走同一份持久 WAL 重试；
// 恢复仍失败时 errors.Join 向上返回，缓存不会继续加载半应用真相。
func (e *Engine) rollbackTruthTransactionLocked(tx *store.TruthTransaction, cause error) error {
	abortErr := tx.Abort()
	e.rt.truthTxActive = false
	e.rt.cache = nil // 精确重读恢复后的 before-image，不赌 mtime/size 一定变化
	reloadErr := e.reloadLocked()
	return errors.Join(cause, abortErr, reloadErr)
}

// commitTruthTransactionLocked 写 committed marker 后重载。返回 committed=false
// 表示尚未越过提交点，调用方必须 rollback；true 表示绝不能再回滚/重试业务操作，
// 即使 err 是 WAL 清理或重载失败。
func (e *Engine) commitTruthTransactionLocked(tx *store.TruthTransaction) (committed bool, err error) {
	committed, commitErr := tx.Commit()
	if !committed {
		return false, commitErr
	}
	e.rt.truthTxActive = false
	e.rt.cache = nil // 写事务提交后精确重读全部 truth，避免快速同尺寸替换漏刷新
	reloadErr := e.reloadLocked()
	return true, errors.Join(commitErr, reloadErr)
}
