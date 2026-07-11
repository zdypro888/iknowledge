package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LocalAuthSession 是长期根密钥经双向 HMAC 握手换得的短期 bearer 替代物。
// 它只存在于进程内/临时 scout 配置；根密钥从不发给未知 listener。
type LocalAuthSession struct {
	Token     string
	ExpiresAt time.Time
}

type localChallengeRequest struct {
	Client string `json:"client"`
	Scope  string `json:"scope"`
}

type localChallengeResponse struct {
	Challenge string `json:"challenge"`
}

type localSessionRequest struct {
	Client    string `json:"client"`
	Challenge string `json:"challenge"`
	Scope     string `json:"scope"`
	Proof     string `json:"proof"`
}

type localSessionResponse struct {
	Session   string `json:"session"`
	ExpiresAt int64  `json:"expires_at"`
	Proof     string `json:"proof"`
}

// AcquireLocalAuthSession 与本机 serve 做 challenge/HMAC/session 双向握手。
// server proof 验证通过前不会发送任何长期密钥或业务请求；鉴权关闭时也必须
// 握手，以免占用可预测端口的无关 listener 冒充知识服务。
func (s *Store) AcquireLocalAuthSession(ctx context.Context, base, scope string, client *http.Client) (LocalAuthSession, error) {
	root, err := s.EnsureLocalIdentity()
	if err != nil {
		return LocalAuthSession{}, err
	}
	base, err = validateLocalAuthBase(base)
	if err != nil {
		return LocalAuthSession{}, err
	}
	if !validLocalAuthScope(scope) {
		return LocalAuthSession{}, fmt.Errorf("store: 非法本机 session scope %q", scope)
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	// HMAC proof 绝不能被 30x 带到另一个 origin；复制 client 只覆盖重定向策略。
	hc := *client
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	clientNonce, err := randomHex32()
	if err != nil {
		return LocalAuthSession{}, fmt.Errorf("store: 生成本机鉴权 nonce: %w", err)
	}
	var challenge localChallengeResponse
	if err := postLocalAuthJSON(ctx, &hc, base+LocalAuthChallengePath,
		localChallengeRequest{Client: clientNonce, Scope: scope}, &challenge); err != nil {
		return LocalAuthSession{}, fmt.Errorf("store: 获取本机鉴权 challenge: %w", err)
	}
	if !ValidLocalAuthValue(challenge.Challenge) {
		return LocalAuthSession{}, fmt.Errorf("store: 本机服务返回非法 challenge")
	}
	clientProof, err := LocalAuthClientProof(root, clientNonce, challenge.Challenge, scope)
	if err != nil {
		return LocalAuthSession{}, err
	}
	var response localSessionResponse
	if err := postLocalAuthJSON(ctx, &hc, base+LocalAuthSessionPath, localSessionRequest{
		Client: clientNonce, Challenge: challenge.Challenge, Scope: scope, Proof: clientProof,
	}, &response); err != nil {
		return LocalAuthSession{}, fmt.Errorf("store: 换取本机短期 session: %w", err)
	}
	if !ValidLocalAuthValue(response.Session) || !ValidLocalAuthValue(response.Proof) {
		return LocalAuthSession{}, fmt.Errorf("store: 本机服务返回非法 session/proof")
	}
	now := time.Now()
	expires := time.Unix(response.ExpiresAt, 0)
	if !expires.After(now) || expires.After(now.Add(2*time.Hour)) {
		return LocalAuthSession{}, fmt.Errorf("store: 本机服务返回异常 session 有效期")
	}
	want, err := LocalAuthServerProof(root, clientNonce, challenge.Challenge, scope, response.Session, response.ExpiresAt)
	if err != nil {
		return LocalAuthSession{}, err
	}
	if !EqualLocalAuthProof(response.Proof, want) {
		return LocalAuthSession{}, fmt.Errorf("store: 本机服务身份校验失败(server proof 不匹配)")
	}
	return LocalAuthSession{Token: response.Session, ExpiresAt: expires}, nil
}

func validLocalAuthScope(scope string) bool {
	return len(scope) > 1 && len(scope) <= 512 && strings.HasPrefix(scope, "/") &&
		!strings.ContainsAny(scope, "?#\r\n\x00")
}

func validateLocalAuthBase(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("store: 非法本机鉴权地址 %q", base)
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("store: 本机 session 握手拒绝非回环地址 %q", u.Host)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func postLocalAuthJSON(ctx context.Context, client *http.Client, endpoint string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8192))
	dec.DisallowUnknownFields()
	if err := dec.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("响应含多余 JSON")
		}
		return err
	}
	return nil
}
