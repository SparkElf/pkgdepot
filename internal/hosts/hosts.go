package hosts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// configSubdir 放在用户配置目录下而不是当前项目目录：主机配置描述的是操作员
// 机器到内网机器的连接关系，属于用户级状态，跟着 cwd 走会导致在不同目录下
// `pkgdepot apply` 看不到已登记的主机。
const configSubdir = "pkgdepot"

// fileName 固定为 hosts.yaml；其它包（CLI、skill export）只通过本包拿路径，
// 不自己拼。
const fileName = "hosts.yaml"

// Path 返回 hosts.yaml 的绝对路径（~/.config/pkgdepot/hosts.yaml 等平台等价路径）。
func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录: %w", err)
	}
	return filepath.Join(base, configSubdir, fileName), nil
}

// Load 读取主机清单。文件不存在时返回空清单而不是错误：首次使用不应要求任何
// 初始化步骤（与 spec.LoadHostsFile 的契约一致）。
func Load() (*spec.HostsFile, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	return spec.LoadHostsFile(p)
}

// Save 整体写回主机清单，权限 0600：条目里虽然没有 SSH 私钥，但记录了内网地址、
// 端口和包清单，属于操作员机器的敏感拓扑信息。
func Save(hf *spec.HostsFile) error {
	if hf == nil {
		return fmt.Errorf("保存主机清单: 内容为空")
	}
	p, err := Path()
	if err != nil {
		return err
	}
	return hf.Save(p)
}

// Add 新增或覆盖同名主机。
//
// 内部拷贝一份再 ApplyDefaults：调用方（CLI）传入的 *spec.Host 通常还会用于
// 打印 URL，这里若原地改它会让调用方看到"被填过默认值"的副作用，行为不直观。
func Add(name string, h *spec.Host) error {
	name, err := normalizeName(name)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("添加主机 %q: 主机信息为空", name)
	}
	hf, err := Load()
	if err != nil {
		return err
	}
	stored := *h
	stored.ApplyDefaults()
	hf.Hosts[name] = &stored
	return Save(hf)
}

// Remove 删除指定主机。未知主机名按错误返回，避免 `host remove` 静默成功。
func Remove(name string) error {
	name, err := normalizeName(name)
	if err != nil {
		return err
	}
	hf, err := Load()
	if err != nil {
		return err
	}
	if _, ok := hf.Hosts[name]; !ok {
		return unknownHost(name, hf)
	}
	delete(hf.Hosts, name)
	return Save(hf)
}

// Get 按名字取主机并补齐默认值。返回的指针已脱离文件内容，调用方可以安全修改
// （例如写 LastApply 后交给 Update）。
func Get(name string) (*spec.Host, error) {
	name, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	hf, err := Load()
	if err != nil {
		return nil, err
	}
	h, err := hf.Get(name)
	if err != nil {
		return nil, err
	}
	stored := *h
	stored.ApplyDefaults()
	return &stored, nil
}

// Update 仅在主机已存在时更新；不存在的名字返回错误而不是隐式新增。
// apply/verify 成功后 CLI 用它回写 LastApply，写错名字必须暴露出来。
func Update(name string, h *spec.Host) error {
	name, err := normalizeName(name)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("更新主机 %q: 主机信息为空", name)
	}
	hf, err := Load()
	if err != nil {
		return err
	}
	if _, ok := hf.Hosts[name]; !ok {
		return unknownHost(name, hf)
	}
	stored := *h
	stored.ApplyDefaults()
	hf.Hosts[name] = &stored
	return Save(hf)
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("主机名不能为空")
	}
	if strings.ContainsAny(name, "/\\ \t\n") {
		return "", fmt.Errorf("主机名 %q 不能包含空白或路径分隔符", name)
	}
	return name, nil
}

func unknownHost(name string, hf *spec.HostsFile) error {
	known := hf.Names()
	if len(known) == 0 {
		return fmt.Errorf("未知主机 %q（尚未配置任何主机，先运行 pkgdepot host add）", name)
	}
	return fmt.Errorf("未知主机 %q（已知: %s）", name, strings.Join(known, ", "))
}
