package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// exportImages 用 docker save 把 registry 镜像打进 bundle。
//
// 目标主机完全离线、拉不到任何镜像，所以镜像必须随 bundle 携带；这也是
// ADR-3 的直接后果。镜像不下载而是 save，因此构建机必须已经 pull 过它们
// ——在这一点上失败比在目标机上失败好得多，错误信息也更可行动。
func exportImages(ctx context.Context, imagesDir string, m *spec.Manifest, log logf) ([]spec.ImageEntry, error) {
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return nil, err
	}
	type imageJob struct {
		role string
		ref  string
		file string
	}
	jobs := []imageJob{
		{role: "verdaccio", ref: defaultString(m.Images.Verdaccio, spec.DefaultVerdaccioImage), file: "verdaccio.tar"},
		{role: "pypiserver", ref: defaultString(m.Images.Pypiserver, spec.DefaultPypiserverImage), file: "pypiserver.tar"},
	}

	entries := make([]spec.ImageEntry, 0, len(jobs))
	for _, job := range jobs {
		// 先确认本地有镜像：docker save 对缺失镜像也只给一句 "unable to find image"，
		// 提前检查能把「先 docker pull」这条指令明确讲出来。
		if _, err := runDockerCmd(ctx, "image", "inspect", job.ref); err != nil {
			return nil, fmt.Errorf("构建机本地没有镜像 %s（%s）：请先执行 `docker pull %s`\n%w",
				job.ref, job.role, job.ref, err)
		}
		dst := filepath.Join(imagesDir, job.file)
		log.printf("导出镜像 %s → images/%s", job.ref, job.file)
		if _, err := runDockerCmd(ctx, "save", "-o", dst, job.ref); err != nil {
			return nil, fmt.Errorf("docker save %s: %w", job.ref, err)
		}
		sum, size, err := fileSHA256(dst)
		if err != nil {
			return nil, fmt.Errorf("计算镜像摘要 %s: %w", dst, err)
		}
		entries = append(entries, spec.ImageEntry{
			Ref:    job.ref,
			File:   "images/" + job.file,
			Size:   size,
			SHA256: sum,
		})
		log.printf("镜像 %s: %s", job.file, humanBytes(size))
	}
	return entries, nil
}
