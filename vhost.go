package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// rootPathRE 约束站点根目录字符集：root 会直接写进 nginx 配置，
// 只允许路径安全字符，拒绝空白、分号、花括号与换行等配置注入面。
var rootPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]*$`)

// vhostStore 封装 conf.d 目录下的独立 vhost 片段与生成清单。
// 所有写操作遵循：先写 .tmp → fsync → rename（同目录原子替换）；
// 删除只允许命中清单内且文件名带本插件前缀的路径，绝不触碰主配置。
type vhostStore struct {
	dir string
}

// newVHostStore 用当前配置的 conf.d 目录构造 store。
func newVHostStore() *vhostStore {
	return &vhostStore{dir: vhostDir()}
}

// manifestPath 返回清单文件路径。
func (s *vhostStore) manifestPath() string {
	return filepath.Join(s.dir, manifestName)
}

// manifestLockPath 返回清单锁文件路径。
//
// 锁**不能**加在清单文件本身上：清单用「.tmp → fsync → rename」原子替换，
// rename 之后清单已是新 inode，而并发实例仍握着旧 inode 的锁——互斥会静默失效。
// 因此锁固定在一个**专用且永不 rename**的旁路文件上（非 .conf 后缀，
// 不会被宿主的 include conf.d/*.conf 收录，也不会被本插件当成 vhost）。
func (s *vhostStore) manifestLockPath() string {
	return s.manifestPath() + ".lock"
}

// ensureDir 确保 conf.d 目录存在（只建目录，不改任何主配置文件）。
func (s *vhostStore) ensureDir() error {
	return os.MkdirAll(s.dir, 0o755)
}

// withManifestLock 在跨进程排他锁内执行 fn：fn 内的「读清单 → 改内存 → 写回」
// 因此成为相对其他实例的原子读-改-写（RMW）。
//
// 只有**写者**需要这把锁：清单写到一半不会暴露给读者——atomicWrite 用 rename 交付，
// 读者（list/get/status/inspect）任何时刻读到的都是某个完整版本，不需要参与加锁。
// 锁文件常驻 conf.d（首次写清单时创建，0600），随实例退出由内核释放锁，不做删除，
// 避免「删除锁文件」与「另一实例 open 同一路径」之间的竞态。
func (s *vhostStore) withManifestLock(fn func() *pluginapi.Error) *pluginapi.Error {
	if err := s.ensureDir(); err != nil {
		if perr := needRootForConfDir(s.dir, err); perr != nil {
			return perr
		}
		return internalErr("创建 conf.d 目录失败: " + err.Error())
	}
	lockFile, err := os.OpenFile(s.manifestLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// 降权后的典型失败点：conf.d 属 root、插件是 kore-site ⇒ EACCES。
		// 这里把「需要 root」翻译成 E_NEED_ROOT + 可操作提示，其余 FS 错误保持 E_INTERNAL。
		if perr := needRootForConfDir(s.dir, err); perr != nil {
			return perr
		}
		return internalErr("打开站点清单锁失败: " + err.Error())
	}
	defer func() { _ = lockFile.Close() }()
	if err := lockExclusive(lockFile); err != nil {
		return internalErr("获取站点清单锁失败: " + err.Error())
	}
	defer func() { _ = unlockFile(lockFile) }()
	return fn()
}

// loadManifest 读清单；不存在返回空清单，损坏则报 E_INTERNAL 不做破坏性覆盖。
func (s *vhostStore) loadManifest() (siteManifest, *pluginapi.Error) {
	b, err := os.ReadFile(s.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return newSiteManifest(), nil
	}
	if err != nil {
		return siteManifest{}, internalErr("读取站点清单失败: " + err.Error())
	}
	var m siteManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return siteManifest{}, internalErr("站点清单损坏: " + err.Error())
	}
	if m.Sites == nil {
		m.Sites = make(map[string]string)
	}
	if m.Version != 1 {
		return siteManifest{}, internalErr("不支持的站点清单版本")
	}
	return m, nil
}

// atomicWrite 先把内容写到同目录临时文件（0600 + fsync），再 rename 覆盖目标。
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kore-site-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("设置临时文件权限: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("原子替换 %s: %w", path, err)
	}
	return nil
}

// validateManagedPath 确认路径确实位于 conf.d 内且文件名带本插件前缀。
// 这是 delete 的硬闸：清单即使被篡改，也无法借相对路径删掉宿主既有 vhost。
func (s *vhostStore) validateManagedPath(path string) error {
	dirAbs, err := filepath.Abs(s.dir)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(dirAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %s 不在 conf.d 目录内", path)
	}
	if !strings.HasPrefix(filepath.Base(path), vhostPrefix) {
		return fmt.Errorf("路径 %s 不带本插件前缀", path)
	}
	return nil
}

// create 生成 domain 的独立 vhost 片段并登记清单。
// 覆盖已存在片段前先写带时间戳备份（同一 conf.d 目录），失败按逆序回滚。
// 返回 (路径, 备份路径, 是否新建, error)。
//
// 清单是**跨进程共享的单一状态**，「读清单 → 落 vhost → 写回清单」整段必须在
// 清单锁内完成——否则多个实例各自读到旧清单再写回，后写者覆盖先写者（丢站点）。
func (s *vhostStore) create(p siteParams) (string, string, bool, *pluginapi.Error) {
	var (
		path    string
		backup  string
		created bool
	)
	perr := s.withManifestLock(func() *pluginapi.Error {
		var lockedErr *pluginapi.Error
		path, backup, created, lockedErr = s.createLocked(p)
		return lockedErr
	})
	if perr != nil {
		return "", "", false, perr
	}
	return path, backup, created, nil
}

// createLocked 是 create 的临界区，调用方必须已持有清单锁。
func (s *vhostStore) createLocked(p siteParams) (string, string, bool, *pluginapi.Error) {
	path := vhostPath(p.Domain)
	if err := s.validateManagedPath(path); err != nil {
		return "", "", false, invalidParam("非法 vhost 路径: " + err.Error())
	}
	if err := s.ensureDir(); err != nil {
		return "", "", false, internalErr("创建 conf.d 目录失败: " + err.Error())
	}
	root := p.Root
	explicitRoot := root != ""
	if root == "" {
		root, _ = defaultSiteRoot(p.Domain)
	}
	port := p.Port
	if port == 0 {
		port = defaultPort
	}
	if port < 1 || port > 65535 {
		return "", "", false, invalidParam("port 必须在 1..65535")
	}
	if err := validateRootPath(root); err != nil {
		return "", "", false, invalidParam("root 非法: " + err.Error())
	}

	m, perr := s.loadManifest()
	if perr != nil {
		return "", "", false, perr
	}
	previous, existed := m.Sites[p.Domain]
	content := vhostTemplate(p.Domain, root, port)
	if existed && previous == path {
		// 覆盖已有托管站点时保留 L2 状态（SSL/伪静态/反代/重定向/index）。
		// root 同理：**没显式写 root 就保留旧根**，绝不因为默认值变化把线上站点根搬走（阶段 C）。
		if old, readErr := os.ReadFile(path); readErr == nil {
			st := parseSiteState(p.Domain, string(old))
			if !explicitRoot {
				root = st.Root
			}
			st.Root, st.Port = root, port
			content = renderVHost(st)
		}
	}
	// 生成后自检：绝不产出 default_server 指令（红队 R6）。
	if strings.Contains(stripComments(content), "default_server") {
		return "", "", false, internalErr("生成的 vhost 含 default_server，拒绝落盘")
	}

	if !existed {
		if _, statErr := os.Stat(path); statErr == nil {
			// 路径上有宿主已有文件但不在清单里：拒绝覆盖，防止误伤。
			return "", "", false, &pluginapi.Error{
				Code:    pluginapi.E_CONFLICT,
				Message: "目标 vhost 已存在但不在本插件清单中，拒绝覆盖: " + path,
			}
		}
	}

	backup := ""
	if existed && previous == path {
		if _, statErr := os.Stat(path); statErr == nil {
			original, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", "", false, internalErr("读取原 vhost 以备份失败: " + readErr.Error())
			}
			// 备份必须与 vhost 同目录：既避免污染 cwd，也保证回滚 rename 同目录原子生效。
			backup = filepath.Join(s.dir, backupName(p.Domain, time.Now()))
			if err := atomicWrite(backup, original); err != nil {
				return "", "", false, internalErr("备份原 vhost 失败: " + err.Error())
			}
		}
	}

	if err := atomicWrite(path, content); err != nil {
		if backup != "" {
			_ = os.Rename(backup, path) // 尽力恢复，不覆盖更严重的错误
		}
		return "", "", false, internalErr("写入 vhost 失败: " + err.Error())
	}

	next := m
	next.Sites = make(map[string]string, len(m.Sites)+1)
	for k, v := range m.Sites {
		next.Sites[k] = v
	}
	next.Sites[p.Domain] = path
	if err := atomicWrite(s.manifestPath(), encode(next)); err != nil {
		// 清单写失败：把片段恢复原状，避免"文件落盘但不在清单"的孤儿。
		if backup != "" {
			_ = os.Rename(backup, path)
		} else {
			_ = os.Remove(path)
		}
		return "", "", false, internalErr("更新站点清单失败: " + err.Error())
	}
	// 重新建站时清掉遗留的停用副本，避免 enable 把旧配置翻回来。
	_ = os.Remove(disabledVHostPath(p.Domain))
	return path, backup, !existed, nil
}

// backupPath 返回本次改动的带时间戳备份路径（与 vhost 同目录、非 .conf 后缀）。
func (s *vhostStore) backupPath(domain string) string {
	return filepath.Join(s.dir, backupName(domain, time.Now()))
}

// delete 只删除清单内、路径校验通过的 vhost 片段；不存在返回 deleted=false。
func (s *vhostStore) delete(domain string) (string, bool, *pluginapi.Error) {
	path, _, deleted, perr := s.deleteWithBackup(domain)
	return path, deleted, perr
}

// deleteWithBackup 在删除前把原片段备份到同目录（非 .conf 后缀，不会被 include），
// 供校验/重载失败时回滚，也留下可审计的删除记录。
// 返回 (路径, 备份路径, 是否真的删除, error)。
//
// 与 create 同理：删 vhost 与摘清单必须与并发写者互斥，否则一次 delete 的
// 「旧清单快照 + 少一条」写回会把它人的新建站点一起抹掉。
func (s *vhostStore) deleteWithBackup(domain string) (string, string, bool, *pluginapi.Error) {
	var (
		path    string
		backup  string
		deleted bool
	)
	perr := s.withManifestLock(func() *pluginapi.Error {
		var lockedErr *pluginapi.Error
		path, backup, deleted, lockedErr = s.deleteLocked(domain)
		return lockedErr
	})
	if perr != nil {
		return path, backup, deleted, perr
	}
	return path, backup, deleted, nil
}

// deleteLocked 是 deleteWithBackup 的临界区，调用方必须已持有清单锁。
func (s *vhostStore) deleteLocked(domain string) (string, string, bool, *pluginapi.Error) {
	path := vhostPath(domain)
	if err := s.validateManagedPath(path); err != nil {
		return "", "", false, invalidParam("非法 vhost 路径: " + err.Error())
	}
	m, perr := s.loadManifest()
	if perr != nil {
		return "", "", false, perr
	}
	managed, ok := m.Sites[domain]
	if !ok {
		// 幂等：没有清单记录就当作已删除，不报错、不碰任何文件。
		return path, "", false, nil
	}
	if managed != path {
		return "", "", false, &pluginapi.Error{
			Code:    pluginapi.E_CONFLICT,
			Message: "清单中的 vhost 路径与当前配置不一致，拒绝删除: " + managed,
		}
	}
	if err := s.validateManagedPath(managed); err != nil {
		return "", "", false, internalErr("清单路径校验失败: " + err.Error())
	}
	backup := ""
	if _, statErr := os.Stat(managed); statErr == nil {
		original, readErr := os.ReadFile(managed)
		if readErr != nil {
			return "", "", false, internalErr("读取待删除 vhost 以备份失败: " + readErr.Error())
		}
		backup = filepath.Join(s.dir, backupName(domain, time.Now()))
		if err := atomicWrite(backup, original); err != nil {
			return "", "", false, internalErr("备份待删除 vhost 失败: " + err.Error())
		}
	}
	if err := os.Remove(managed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", false, internalErr("删除 vhost 失败: " + err.Error())
	}
	next := m
	next.Sites = make(map[string]string, len(m.Sites))
	for k, v := range m.Sites {
		if k != domain {
			next.Sites[k] = v
		}
	}
	if err := atomicWrite(s.manifestPath(), encode(next)); err != nil {
		// 清单写失败：把片段恢复，避免"文件被删但清单还记着"的悬挂状态。
		if backup != "" {
			if restoreErr := os.Rename(backup, managed); restoreErr != nil {
				return "", "", false, internalErr("更新站点清单失败且恢复 vhost 失败: " + err.Error() + "; " + restoreErr.Error())
			}
		}
		return "", "", false, internalErr("更新站点清单失败: " + err.Error())
	}
	// 停用态副本也应随删除一并清理，避免 conf.d 里留下无法管理的孤儿文件。
	_ = os.Remove(disabledVHostPath(domain))
	return path, backup, true, nil
}

// validateRootPath 校验站点根目录：绝对路径、无冗余分量、字符集安全。
// root 会原样进入 nginx 配置，这是防配置注入的硬闸，不只是路径合法性检查。
func validateRootPath(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("必须是绝对路径")
	}
	if root != filepath.Clean(root) {
		return errors.New("不能含冗余路径分量")
	}
	if !rootPathRE.MatchString(root) {
		return errors.New("仅允许字母数字与 ._/@+-，禁止空白、分号、花括号与换行")
	}
	return nil
}

// fileSnapshot 记录单个文件被改动前的字节与权限，用于校验失败后的原样恢复。
type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

// snapshotFile 读取文件当前状态；不存在也是合法状态（存在=false）。
func snapshotFile(path string) (fileSnapshot, error) {
	st, statErr := os.Stat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if statErr != nil {
		return fileSnapshot{}, statErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{path: path, data: data, mode: st.Mode().Perm(), exists: true}, nil
}

// restore 把文件恢复成快照状态：原本存在则原子写回原字节与权限，原本不存在则删除。
func (s fileSnapshot) restore() error {
	if !s.exists {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除 %s: %w", s.path, err)
		}
		return nil
	}
	if err := atomicWrite(s.path, s.data); err != nil {
		return fmt.Errorf("恢复 %s: %w", s.path, err)
	}
	if s.mode != 0 {
		if err := os.Chmod(s.path, s.mode); err != nil {
			return fmt.Errorf("恢复 %s 权限: %w", s.path, err)
		}
	}
	return nil
}

// list 从清单返回当前已被本插件管理的站点。
func (s *vhostStore) list() ([]siteInfo, *pluginapi.Error) {
	m, perr := s.loadManifest()
	if perr != nil {
		return nil, perr
	}
	sites := make([]siteInfo, 0, len(m.Sites))
	for domain, path := range m.Sites {
		enabled := false
		if _, err := os.Stat(path); err == nil {
			enabled = true
		}
		disabled := false
		if !enabled {
			// 停用态：主 vhost 改名成 .conf.disabled，站点仍算"存在"。
			if _, err := os.Stat(disabledVHostPath(domain)); err == nil {
				disabled = true
			}
		}
		info := siteInfo{
			Domain:    domain,
			VHostPath: path,
			Enabled:   enabled,
			Disabled:  disabled,
			Exists:    enabled || disabled,
		}
		if content, err := os.ReadFile(path); err == nil {
			root := parseRootDirective(string(content))
			if siteRootOutsideInstall(root) {
				info.OutsideInstall = true
				info.OutsideInstallHint = outsideInstallHint()
			}
		}
		sites = append(sites, info)
	}
	return sites, nil
}

// stripComments 去掉以 # 开头的整行，供 default_server 自检避免注释误报。
func stripComments(content []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// internalErr 构造 E_INTERNAL 错误体。
func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}
