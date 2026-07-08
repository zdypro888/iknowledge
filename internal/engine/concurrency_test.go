package engine

import (
	"fmt"
	"sync"
	"testing"
)

// R29 批次2 并发压测:多 goroutine 同时 Recall/Map/Inject(读路径),验证
// RWMutex 改造后读读并发无 race(持锁/放锁正确)、无死锁(-race 下完成)。
// 读路径改用 RLock 后,这些调用应全部成功返回。
func TestConcurrentReadsNoRace(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	// 预置一条知识,让 recall 有实质内容渲染。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "登录入口"}},
	}, "seeder", "codex"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("reader-%d", i)
			for j := 0; j < 20; j++ {
				// Recall:命中节点路径
				if _, _, err := e.Recall(RecallArgs{Query: "internal/auth/login.go#Login"}, sid); err != nil {
					errs <- err
					return
				}
				// Map:金字塔只读
				if _, _, err := e.Map("internal/auth", 2, sid); err != nil {
					errs <- err
					return
				}
				// Inject:hook 注入路径
				if _, err := e.Inject("internal/auth/login.go", sid, "Read"); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发读失败: %v", err)
	}
}

// 读写混合:一边持续 Recall,一边做 Remember(record_change 经写锁),
// 验证 RWMutex 下读写不冲突、不死锁、数据不腐(-race 把关)。
func TestConcurrentReadWriteMix(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	stop := make(chan struct{})

	// 读者:持续 recall 直到 stop
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("r%d", i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := e.Recall(RecallArgs{Query: "internal/auth/login.go#Login"}, sid); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	// 写者:做若干次 remember(每次取写锁 + reload)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if _, err := e.Remember(RememberArgs{
				Node:    "internal/auth/login.go#Login",
				Entries: []RememberEntry{{Kind: "pitfall", Text: fmt.Sprintf("注意点 %d", i)}},
			}, "writer", "codex"); err != nil {
				errs <- err
				return
			}
		}
		close(stop)
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("读写混合失败: %v", err)
	}
}
