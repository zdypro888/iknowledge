package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 鉴权令牌(impl §1 --auth 定案,2026-07-04 自四期提前):serve --auth 生成随机
// token 写 .knowledge/local/token(0600),客户端经 Authorization: Bearer 携带。
// 威胁模型:共享多用户机器上,同机其他用户经回环端口读写库、伪造 clientInfo
// 冒充 author;明文 HTTP 的网络窃听不在此缓解范围(仅回环时无此暴露)。
// local/ 属可再生层:token 丢失重启 serve 即重生成,不 fsync。

func (s *Store) authTokenPath() string { return filepath.Join(s.dir, "local", "token") }

// EnsureAuthToken 读取或生成鉴权令牌(幂等;0600——同机其他用户不可读)。
func (s *Store) EnsureAuthToken() (string, error) {
	if tok, err := s.LoadAuthToken(); err != nil {
		return "", err
	} else if tok != "" {
		return tok, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: 生成 token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(s.authTokenPath()), 0o755); err != nil {
		return "", fmt.Errorf("store: 建 local 目录: %w", err)
	}
	if err := os.WriteFile(s.authTokenPath(), []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("store: 写 token: %w", err)
	}
	return tok, nil
}

// LoadAuthToken 读令牌;文件不存在返回 ("", nil)(setup/hook 用:有则携带,无则裸连)。
func (s *Store) LoadAuthToken() (string, error) {
	data, err := os.ReadFile(s.authTokenPath())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: 读 token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
