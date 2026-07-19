package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 鉴权根密钥与 scout 信任属于“本机授权状态”，不能放在会随仓库复制、归档或
// 由项目 ACL 暴露的 .knowledge/local/。它们按 canonical repo path 的 SHA-256
// 分仓，写到用户 profile 下的私有状态目录。IKNOWLEDGE_STATE_HOME 供受控部署与
// 测试覆盖；正常安装使用 os.UserConfigDir（Windows 即当前用户的 AppData）。
const (
	stateHomeEnv        = "IKNOWLEDGE_STATE_HOME"
	stateAppDir         = "iknowledge/state"
	authTokenStateFile  = "auth-token"
	localIdentityFile   = "local-identity"
	scoutTrustStateFile = "scout-trust-v1"
	semanticStateFile   = "semantic-config-v1.json"
	legacyAuthTokenRel  = "local/token"
	legacyScoutTrustRel = "local/scout-trust-v1"

	LocalSessionAuthScheme = "IKnowledgeSession"
	localAuthDomain        = "iknowledge-local-auth-v1"
	LocalAuthChallengePath = "/auth/local/challenge"
	LocalAuthSessionPath   = "/auth/local/session"
)

// PrivateStateDir 返回本仓库的本机私有状态目录。调用方只应用它作诊断展示；
// 文件读写必须继续走本文件的安全入口。
func (s *Store) PrivateStateDir() (string, error) {
	base := strings.TrimSpace(os.Getenv(stateHomeEnv))
	if base == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("store: 定位用户配置目录: %w", err)
		}
		base = filepath.Join(configDir, filepath.FromSlash(stateAppDir))
	} else if !filepath.IsAbs(base) {
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("store: 解析 %s: %w", stateHomeEnv, err)
		}
		base = abs
	}
	var err error
	base, err = canonicalStateRoot(base)
	if err != nil {
		return "", err
	}

	repo := filepath.Clean(s.repo)
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repo = filepath.Clean(resolved)
	}
	if runtime.GOOS == "windows" {
		// Windows 路径大小写不敏感；同一仓库不能因调用方盘符大小写不同分裂状态。
		repo = strings.ToLower(repo)
	}
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(base, "repos", hex.EncodeToString(sum[:])), nil
}

// canonicalStateRoot 把用户选择的 state-home（信任根）解析到最长既存祖先的
// 真实路径，再原样接回尚不存在的尾部。这样 macOS 的 /var -> /private/var 等
// 正常系统布局不会被误伤，而信任根之下的 repos/<repo-key> 仍可逐级拒绝链接。
func canonicalStateRoot(path string) (string, error) {
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("store: 解析本机状态根: %w", err)
	}
	probe := clean
	var suffix []string
	for {
		_, statErr := os.Lstat(probe)
		if statErr == nil {
			resolved, evalErr := filepath.EvalSymlinks(probe)
			if evalErr != nil {
				return "", fmt.Errorf("store: 解析本机状态根链接: %w", evalErr)
			}
			resolvedInfo, evalErr := os.Stat(resolved)
			if evalErr != nil || !resolvedInfo.IsDir() {
				return "", fmt.Errorf("store: 本机状态根祖先不是目录: %s", probe)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("store: 检查本机状态根: %w", statErr)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("store: 找不到本机状态根的既存祖先: %s", clean)
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func (s *Store) privateStatePath(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("store: 非法本机状态文件名 %q", name)
	}
	dir, err := s.PrivateStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func (s *Store) authTokenPath() (string, error) {
	return s.privateStatePath(authTokenStateFile)
}

func (s *Store) localIdentityPath() (string, error) {
	return s.privateStatePath(localIdentityFile)
}

// AuthTokenFile 返回显式 HTTP-direct 用户查找根 token 的位置。
func (s *Store) AuthTokenFile() (string, error) { return s.authTokenPath() }

// EnsureLocalIdentity 返回内部 loopback 客户端验证 serve 身份的长期随机密钥。
// 它与显式 HTTP Bearer 根 token 分离：即使用户关闭 --auth，stdio/hook/scout
// 也必须先证明端口上的进程确是本仓库 serve，不能只信一个可伪造的 /status 200。
func (s *Store) EnsureLocalIdentity() (string, error) {
	path, err := s.localIdentityPath()
	if err != nil {
		return "", err
	}
	if data, readErr := readPrivateStateFile(path); readErr == nil {
		identity, err := validateRootToken(string(data))
		if err != nil {
			return "", fmt.Errorf("store: 本机身份密钥损坏: %w", err)
		}
		return identity, nil
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("store: 读本机身份密钥: %w", readErr)
	}
	identity, err := randomHex32()
	if err != nil {
		return "", fmt.Errorf("store: 生成本机身份密钥: %w", err)
	}
	created, err := createPrivateStateFileExclusive(path, []byte(identity+"\n"))
	if err != nil {
		return "", fmt.Errorf("store: 写本机身份密钥: %w", err)
	}
	if created {
		return identity, nil
	}
	// 另一个进程赢得首次创建。等待它完成原子发布后统一返回胜者值；若其
	// 发布损坏内容则最终 fail closed，绝不悄悄轮换成第二个身份。
	return waitForPrivateRootToken(path, "本机身份密钥")
}

func (s *Store) legacyStatePath(rel string) (string, error) {
	return s.knowledgePath(rel)
}

func ensurePrivateStateDir(dir string) error {
	dir = filepath.Clean(dir)
	// privateStatePath 的固定形状是 <trusted-root>/repos/<repo-key>。只把
	// trusted-root 当边界；其下两个组件逐级 Lstat/Mkdir，绝不让 MkdirAll
	// 先穿过攻击者预置的 repos symlink。
	root := filepath.Dir(filepath.Dir(dir))
	if root == dir || root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("store: 非法本机私有状态目录: %s", dir)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("store: 建本机私有状态根: %w", err)
	}
	components := []string{root, filepath.Join(root, filepath.Base(filepath.Dir(dir))), dir}
	for i, component := range components {
		info, err := os.Lstat(component)
		if os.IsNotExist(err) && i > 0 {
			if err := os.Mkdir(component, 0o700); err != nil && !os.IsExist(err) {
				return fmt.Errorf("store: 建本机私有状态目录: %w", err)
			}
			info, err = os.Lstat(component)
		}
		if err != nil {
			return fmt.Errorf("store: 检查本机私有状态目录: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("store: 本机私有状态路径必须是真目录: %s", component)
		}
		if runtime.GOOS != "windows" {
			// state-home 是用户选择的信任根，不能擅自 chmod 一个可能是
			// /tmp 的既存共享目录；真正承载秘密的后两层始终收紧为 0700。
			if i > 0 {
				if err := os.Chmod(component, 0o700); err != nil {
					return fmt.Errorf("store: 收紧本机私有状态目录权限: %w", err)
				}
			}
		}
	}
	return nil
}

func writePrivateStateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateStateDir(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("store: 本机状态目标必须是普通文件: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: 检查本机状态目标: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return fmt.Errorf("store: 建本机状态临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("store: chmod 本机状态: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("store: 写本机状态: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("store: fsync 本机状态: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: 关闭本机状态: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: 本机状态目标在提交前变为非普通文件: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: 提交本机状态: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("store: 收紧本机状态权限: %w", err)
		}
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: fsync 本机状态目录: %w", err)
	}
	return nil
}

// createPrivateStateFileExclusive 供“首次生成且所有并发调用必须看到同一值”的
// 根密钥使用。先完整 fsync 0600 临时文件，再用 hard-link 原子、不可覆盖地发布；
// 因此输家只会看到完整胜者值，不存在 O_EXCL 后直接写最终文件的半成品窗口。
func createPrivateStateFileExclusive(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := ensurePrivateStateDir(dir); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := f.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	var opErr error
	if err := f.Chmod(0o600); err != nil {
		opErr = err
	} else if _, err := f.Write(data); err != nil {
		opErr = err
	} else if err := f.Sync(); err != nil {
		opErr = err
	}
	closeErr := f.Close()
	if opErr != nil || closeErr != nil {
		return false, errors.Join(opErr, closeErr)
	}
	if err := os.Link(tmpName, path); os.IsExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Remove(tmpName); err != nil {
		return true, fmt.Errorf("清理已发布根密钥临时链接: %w", err)
	}
	tmpName = ""
	if err := fsyncDir(dir); err != nil {
		return true, err
	}
	return true, nil
}

func readPrivateStateFile(path string) ([]byte, error) {
	return readPrivateStateFileLimit(path, 4096)
}

func readPrivateStateFileLimit(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 || maxBytes > 1<<20 {
		return nil, fmt.Errorf("store: 本机状态读取上限非法: %d", maxBytes)
	}
	if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store: 本机状态必须是普通文件: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("store: 收紧本机状态权限: %w", err)
		}
		info, err = os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, fmt.Errorf("store: 本机状态权限未能收紧为 0600: %s", path)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("store: 本机状态文件异常过大: %s", path)
	}
	return data, nil
}

func waitForPrivateRootToken(path, label string) (string, error) {
	var lastErr error
	for i := 0; i < 100; i++ {
		data, err := readPrivateStateFile(path)
		if err == nil {
			if token, validateErr := validateRootToken(string(data)); validateErr == nil {
				return token, nil
			} else {
				lastErr = validateErr
			}
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", fmt.Errorf("store: %s 并发创建后不可读或损坏: %w", label, lastErr)
}

func randomHex32() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func validateRootToken(tok string) (string, error) {
	tok = strings.TrimSpace(tok)
	decoded, err := hex.DecodeString(tok)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("token 应为 32 字节随机值的 64 位 hex")
	}
	return tok, nil
}

// EnsureAuthToken 读取或生成仓库的长期根密钥。根密钥永不写回仓库。
func (s *Store) EnsureAuthToken() (string, error) {
	if tok, err := s.LoadAuthToken(); err != nil {
		return "", err
	} else if tok != "" {
		return tok, nil
	}
	tok, err := randomHex32()
	if err != nil {
		return "", fmt.Errorf("store: 生成 token: %w", err)
	}
	path, err := s.authTokenPath()
	if err != nil {
		return "", err
	}
	created, err := createPrivateStateFileExclusive(path, []byte(tok+"\n"))
	if err != nil {
		return "", fmt.Errorf("store: 写 token: %w", err)
	}
	if created {
		return tok, nil
	}
	return waitForPrivateRootToken(path, "鉴权根 token")
}

// LoadAuthToken 读取长期根密钥；不存在返回 ("", nil)。旧版仓内 token 只作为
// “auth 模式曾启用”的迁移信号：绝不复用可随项目传播的旧 secret，而是轮换为
// 全新外部根密钥后删除 legacy，避免恶意仓库预置一个攻击者已知的 token。
func (s *Store) LoadAuthToken() (string, error) {
	path, err := s.authTokenPath()
	if err != nil {
		return "", err
	}
	data, readErr := readPrivateStateFile(path)
	if readErr == nil {
		tok, err := validateRootToken(string(data))
		if err != nil {
			return "", fmt.Errorf("store: 外部 token 损坏: %w", err)
		}
		if err := s.removeLegacyState(legacyAuthTokenRel); err != nil {
			return "", err
		}
		return tok, nil
	}
	if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("store: 读外部 token: %w", readErr)
	}

	legacy, err := s.legacyStatePath(legacyAuthTokenRel)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(legacy); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("store: legacy token 必须是普通文件，拒绝迁移: %s", legacy)
		}
		// 安全轮换，不读取或复用仓内 secret。
		tok, randErr := randomHex32()
		if randErr != nil {
			return "", fmt.Errorf("store: 迁移时生成新 token: %w", randErr)
		}
		if err := writePrivateStateFile(path, []byte(tok+"\n")); err != nil {
			return "", err
		}
		if err := s.removeLegacyState(legacyAuthTokenRel); err != nil {
			return "", err
		}
		return tok, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("store: 检查 legacy token: %w", err)
	}
	return "", nil
}

func (s *Store) removeLegacyState(rel string) error {
	path, err := s.legacyStatePath(rel)
	if err != nil {
		return err
	}
	_, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.RemoveKnowledgeFile(rel); err != nil {
		return fmt.Errorf("store: 删除 legacy 本机状态 %s: %w", rel, err)
	}
	return nil
}

// WriteScoutTrust 写本仓库的本机 scout 授权，并删除仓内 legacy marker。
func (s *Store) WriteScoutTrust(fingerprint string) error {
	path, err := s.privateStatePath(scoutTrustStateFile)
	if err != nil {
		return err
	}
	if err := writePrivateStateFile(path, []byte(strings.TrimSpace(fingerprint)+"\n")); err != nil {
		return err
	}
	return s.removeLegacyState(legacyScoutTrustRel)
}

// LoadScoutTrust 只信任仓外 marker。若发现 legacy marker，立即删除并返回未授权；
// 不能自动复制其可预测内容，否则打包仓库仍能预置进程执行授权。
func (s *Store) LoadScoutTrust() (string, error) {
	path, err := s.privateStatePath(scoutTrustStateFile)
	if err != nil {
		return "", err
	}
	data, readErr := readPrivateStateFile(path)
	if err := s.removeLegacyState(legacyScoutTrustRel); err != nil {
		return "", err
	}
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", os.ErrNotExist
		}
		return "", readErr
	}
	return strings.TrimSpace(string(data)), nil
}

// SemanticConfigFile 返回本仓库语义检索本机配置的位置，仅用于状态展示。
// endpoint/model/启用开关都不进入仓库，避免恶意分支借配置触发内容外发。
func (s *Store) SemanticConfigFile() (string, error) {
	return s.privateStatePath(semanticStateFile)
}

// WriteSemanticConfig 写语义检索的仓外、本机、按 canonical repo 隔离的配置。
// API key 不属于该文件；调用方只从固定的进程环境变量读取。
func (s *Store) WriteSemanticConfig(data []byte) error {
	if len(data) == 0 || len(data) > 64<<10 {
		return fmt.Errorf("store: semantic 配置大小必须在 1..65536 字节")
	}
	path, err := s.privateStatePath(semanticStateFile)
	if err != nil {
		return err
	}
	return writePrivateStateFile(path, data)
}

// LoadSemanticConfig 只读仓外语义检索配置；不存在返回 os.ErrNotExist。
func (s *Store) LoadSemanticConfig() ([]byte, error) {
	path, err := s.privateStatePath(semanticStateFile)
	if err != nil {
		return nil, err
	}
	return readPrivateStateFileLimit(path, 64<<10)
}

// RemoveSemanticConfig 删除本机语义检索配置。向量缓存另在 .knowledge/local，
// 调用方可选择保留（再次启用时校验指纹）或单独清理。
func (s *Store) RemoveSemanticConfig() error {
	path, err := s.privateStatePath(semanticStateFile)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("store: semantic 配置必须是普通文件: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(path))
}

// LocalAuthClientProof / LocalAuthServerProof 是内部 loopback 握手的域分离 HMAC。
// 根密钥永不出客户端；服务端必须返回不同域的 proof，恶意 listener 无法把刚
// 收到的 client proof 反射成合法响应。
func LocalAuthClientProof(rootToken, clientNonce, challenge, scope string) (string, error) {
	return localAuthMAC(rootToken, "client", clientNonce, challenge, scope)
}

func LocalAuthServerProof(rootToken, clientNonce, challenge, scope, session string, expiresUnix int64) (string, error) {
	return localAuthMAC(rootToken, "server", clientNonce, challenge, scope, session, fmt.Sprintf("%d", expiresUnix))
}

func localAuthMAC(rootToken string, fields ...string) (string, error) {
	rootToken, err := validateRootToken(rootToken)
	if err != nil {
		return "", err
	}
	key, _ := hex.DecodeString(rootToken)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(localAuthDomain))
	for _, field := range fields {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func ValidLocalAuthValue(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func EqualLocalAuthProof(got, want string) bool {
	gotBytes, gotErr := hex.DecodeString(got)
	wantBytes, wantErr := hex.DecodeString(want)
	if gotErr != nil || wantErr != nil || len(gotBytes) != sha256.Size || len(wantBytes) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(gotBytes, wantBytes) == 1
}

func LocalSessionAuthorization(session string) string {
	return LocalSessionAuthScheme + " " + session
}

// IsNoAuthState 是握手客户端区分“未启用 auth”与真实状态错误的辅助。
func IsNoAuthState(err error) bool { return errors.Is(err, os.ErrNotExist) }
