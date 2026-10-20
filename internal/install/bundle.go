package install

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// maxManifestSize 限制从 tar 里读 manifest.json 的字节数：bundles 可能很大，
// 但清单本身只有几十 KB。设上限是为了防止一个畸形 bundle 让 CLI 内存爆掉。
const maxManifestSize = 8 << 20 // 8 MiB

// readBundleManifest 直接从 bundle tar.gz 里读出 manifest.json，不解压其余内容。
//
// 不依赖 internal/bundle 的 ReadManifest：按架构约定各包只依赖 internal/spec，
// 部署侧只需要这一小块能力，重复二十行代码比引入对构建侧的编译依赖更稳妥
// （部署侧二进制可能先于构建侧代码存在）。
func readBundleManifest(bundlePath string) (*spec.BundleManifest, error) {
	_, bm, err := readBundle(bundlePath)
	return bm, err
}

// readBundle 同时返回 manifest.json 的原始字节与解析结果。
// apply 需要把原始字节原样上传到目标机：重新序列化会改变字节，让目标机上的
// manifest.json 与 bundle 内的权威副本产生 no-op 差异。
func readBundle(bundlePath string) ([]byte, *spec.BundleManifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 bundle %s: %w", bundlePath, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("bundle %s 不是合法的 gzip: %w", bundlePath, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("bundle %s 里没有 manifest.json", bundlePath)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("读取 bundle %s: %w", bundlePath, err)
		}
		// tar 里可能带 "./" 前缀（取决于打包方式），统一规范化后比较。
		name := strings.TrimPrefix(path.Clean(hdr.Name), "./")
		if name != spec.RemoteManifestFile {
			continue
		}
		if hdr.Size > maxManifestSize {
			return nil, nil, fmt.Errorf("bundle %s 的 manifest.json 异常大（%d 字节）", bundlePath, hdr.Size)
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxManifestSize))
		if err != nil {
			return nil, nil, fmt.Errorf("读取 bundle %s 中的 manifest.json: %w", bundlePath, err)
		}
		var bm spec.BundleManifest
		// bundle manifest 由构建侧用 JSON 写出，必须按 JSON 解码：用 yaml 解码
		// 会把 createdAt 之类的字段静默解成零值。
		if err := json.Unmarshal(raw, &bm); err != nil {
			return nil, nil, fmt.Errorf("解析 bundle %s 的 manifest.json: %w", bundlePath, err)
		}
		if bm.APIVersion != spec.BundleAPIVersion {
			return nil, nil, fmt.Errorf("bundle manifest apiVersion 必须是 %q，实际 %q",
				spec.BundleAPIVersion, bm.APIVersion)
		}
		return raw, &bm, nil
	}
}
