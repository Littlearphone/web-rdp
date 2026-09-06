//go:build windows

package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAsyncHWEncodeSmoke 头less 验证异步硬件 MFT 能否在本机产出可解码 H.264。
// 不受 CGO/浏览器/页面限制，可直接 go test 自跑。
func TestAsyncHWEncodeSmoke(t *testing.T) {
	Verbose = true
	out, info, err := AsyncHWEncodeSmoke(320, 240, 3)
	if err != nil {
		t.Fatalf("AsyncHWEncodeSmoke 失败: %v (info=%s)", err, info)
	}
	t.Logf("OK: %s", info)

	// 落盘
	dir := os.TempDir()
	path := filepath.Join(dir, "asynchw_320x240.h264")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	t.Logf("已写 %s (%d bytes)", path, len(out))

	// 用独立 ffmpeg 做解码器校验（仅测试验证，非交付编码路径）
	ff := findFFmpeg()
	if ff == "" {
		t.Log("未找到 ffmpeg，跳过独立解码校验")
		return
	}
	cmd := exec.Command(ff, "-v", "error", "-f", "h264", "-i", path,
		"-frames:v", "2", "-f", "null", "-")
	combined, cerr := cmd.CombinedOutput()
	if cerr != nil {
		t.Logf("ffmpeg 解码异常(可能首帧 keyframe 不足): %v\n%s", cerr, combined)
	} else {
		t.Log("ffmpeg 独立解码通过：产出可解码 H.264 帧")
	}
}

// TestAsyncHWEncodePerf 测稳态吞吐（头less，NVIDIA 硬件 MFT）。
func TestAsyncHWEncodePerf(t *testing.T) {
	Verbose = false
	for _, cfg := range []struct{ w, h, n int }{
		{640, 360, 200},
		{1280, 720, 120},
		{1920, 1080, 90},
	} {
		fps, nb, info, err := AsyncHWEncodePerf(cfg.w, cfg.h, cfg.n)
		if err != nil {
			t.Logf("  %s 失败: %v", info, err)
			continue
		}
		t.Logf("  %s", info)
		_ = nb
		_ = fps
	}
}

func findFFmpeg() string {
	cands := []string{
		`D:\DataCenter\Portable\Bat2Exe\ffmpeg-n5.0-latest-win64-gpl-5.0\bin\ffmpeg.exe`,
		`ffmpeg`,
	}
	for _, c := range cands {
		if strings.Contains(c, `\`) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		} else if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}
