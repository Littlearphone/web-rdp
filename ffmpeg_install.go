package main

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ffmpegLocalDir = "ffmpeg_local"
const ffmpegReleaseAPI = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/latest"

var (
	ffmpegPath   string
	hasDXGI      bool
	useFFmpeg    bool
	h264Encoders []string // 可用编码器列表（GPU优先，CPU回退）
	h264EncIdx   int      // 当前使用的编码器索引
)

// 返回当前选中的 H.264 编码器名称
func currentH264Encoder() string {
	if h264EncIdx < len(h264Encoders) {
		return h264Encoders[h264EncIdx]
	}
	return ""
}

// 编码器失败时切换到下一个可用编码器，返回是否还有可用选项
func tryNextH264Encoder() bool {
	h264EncIdx++
	if h264EncIdx < len(h264Encoders) {
		log.Printf("H.264 编码器回退 → %s", h264Encoders[h264EncIdx])
		return true
	}
	return false
}

func detectFFmpeg() {
	local := filepath.Join(ffmpegLocalDir, "bin", "ffmpeg.exe")
	if _, err := os.Stat(local); err == nil {
		ffmpegPath = local
		hasDXGI = checkDXGI(local)
		useFFmpeg = true
		return
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpegPath = p
		hasDXGI = checkDXGI(p)
		useFFmpeg = true
		if !hasDXGI {
			if askYN("当前 ffmpeg 不支持 dxgigrab，下载优化版？") {
				downloadAndExtract()
				if _, err := os.Stat(local); err == nil {
					ffmpegPath = local
					hasDXGI = checkDXGI(local)
				}
			}
		}
		return
	}
	if askYN("未找到 ffmpeg，自动下载？") {
		downloadAndExtract()
		if _, err := os.Stat(local); err == nil {
			ffmpegPath = local
			hasDXGI = checkDXGI(local)
			useFFmpeg = true
			return
		}
	}
	useFFmpeg = false
	fmt.Println("→ 使用纯 Go 截图方案（无 ffmpeg）")
}

// 通过 wmic 检测主显卡品牌，避免盲目尝试不兼容的 GPU 编码器
func detectGPUVendor() string {
	out, err := exec.Command("wmic", "path", "win32_VideoController", "get", "name").Output()
	if err != nil {
		return ""
	}
	s := strings.ToLower(string(out))
	if strings.Contains(s, "nvidia") {
		return "nvidia"
	}
	if strings.Contains(s, "amd") || strings.Contains(s, "radeon") {
		return "amd"
	}
	if strings.Contains(s, "intel") {
		return "intel"
	}
	return ""
}

// 按 GPU 品牌 + ffmpeg 编码器可用性构建编码器列表。
// 优先匹配 GPU 的硬件编码器，然后是 CPU libx264（画质好），最后才是系统 MF 编码器。
func detectH264Encoder() {
	if len(h264Encoders) > 0 {
		return
	}
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return
	}
	s := string(out)
	vendor := detectGPUVendor()
	// 只添加匹配当前 GPU 品牌的硬件编码器
	switch vendor {
	case "nvidia":
		if strings.Contains(s, "h264_nvenc") {
			h264Encoders = append(h264Encoders, "h264_nvenc")
		}
	case "amd":
		if strings.Contains(s, "h264_amf") {
			h264Encoders = append(h264Encoders, "h264_amf")
		}
	case "intel":
		if strings.Contains(s, "h264_qsv") {
			h264Encoders = append(h264Encoders, "h264_qsv")
		}
	}
	// CPU 软件编码（画质最好，兼容性最广）
	if strings.Contains(s, "libx264") {
		h264Encoders = append(h264Encoders, "libx264")
	}
	// Windows Media Foundation 作为最后兜底
	if strings.Contains(s, "h264_mf") {
		h264Encoders = append(h264Encoders, "h264_mf")
	}
	if len(h264Encoders) > 0 {
		log.Printf("GPU: %s → H.264 编码器: %s (共 %d 个)", vendor, h264Encoders[0], len(h264Encoders))
	} else {
		log.Printf("GPU: %s → 未找到可用 H.264 编码器", vendor)
	}
}

func askYN(prompt string) bool {
	fmt.Printf("\n⚠ %s [Y/n]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}

func resolveDownloadURL() string {
	resp, err := httpClient.Get(ffmpegReleaseAPI)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&release) != nil {
		return ""
	}
	for _, a := range release.Assets {
		if strings.Contains(a.Name, "win64") && strings.Contains(a.Name, "gpl") && strings.HasSuffix(a.Name, ".zip") {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func checkDXGI(path string) bool {
	out, err := exec.Command(path, "-hide_banner", "-devices").Output()
	return err == nil && strings.Contains(string(out), "dxgigrab")
}

func downloadAndExtract() {
	tmpFile := filepath.Join(os.TempDir(), "ffmpeg_download.zip")
	defer func() { _ = os.Remove(tmpFile) }()
	for attempt := 1; ; attempt++ {
		dlURL := resolveDownloadURL()
		if dlURL == "" {
			fmt.Println("  无法获取下载地址")
			fmt.Println("  请前往: https://github.com/BtbN/FFmpeg-Builds/releases")
			fmt.Println("  选择 win64-gpl.zip 下载到本地，解压后使用 -ffmpeg 指定路径")
			fmt.Println("  或在命令提示符中执行: winget install ffmpeg")
			return
		}
		fmt.Printf("下载 ffmpeg... (约 120MB)\n")
		resp, err := httpClient.Get(dlURL)
		if err != nil {
			fmt.Printf("  下载失败: %v\n", err)
			if askYN("重试下载？") {
				continue
			}
			return
		}
		f, _ := os.Create(tmpFile)
		totalSize, downloaded, startTime := resp.ContentLength, int64(0), time.Now()
		buf, lastReport := make([]byte, 64*1024), time.Now()
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = f.Write(buf[:n])
				downloaded += int64(n)
				if totalSize > 0 && time.Since(lastReport) > 200*time.Millisecond {
					elapsed := time.Since(startTime).Seconds()
					speed := float64(downloaded) / elapsed / 1024 / 1024
					pct := downloaded * 100 / totalSize
					eta := ""
					if speed > 0 {
						eta = fmt.Sprintf(" 剩余 %ds", int(float64(totalSize-downloaded)/(speed*1024*1024)))
					}
					fmt.Printf("\r  %d%%  %.1f MB/s%s    ", pct, speed, eta)
					lastReport = time.Now()
				} else if totalSize <= 0 {
					fmt.Printf("\r  已下载 %.1f MB    ", float64(downloaded)/1024/1024)
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = f.Close()
		_ = resp.Body.Close()
		if totalSize > 0 && downloaded < totalSize {
			fmt.Printf("\n  下载不完整\n")
			if askYN("重试下载？") {
				continue
			}
			return
		}
		fmt.Printf("\n  下载完成 (%.1f MB)\n", float64(downloaded)/1024/1024)
		fmt.Println("解压...")
		_ = os.RemoveAll(ffmpegLocalDir)
		zr, err := zip.OpenReader(tmpFile)
		if err != nil {
			fmt.Printf("  解压失败\n")
			if askYN("重试下载？") {
				continue
			}
			return
		}
		ok := true
		for _, zf := range zr.File {
			parts := strings.SplitN(zf.Name, "/", 2)
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			target := filepath.Join(ffmpegLocalDir, parts[1])
			if zf.FileInfo().IsDir() {
				_ = os.MkdirAll(target, 0755)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0755)
			rc, e1 := zf.Open()
			out, e2 := os.Create(target)
			if e1 != nil || e2 != nil {
				ok = false
				break
			}
			_, _ = io.Copy(out, rc)
			_ = rc.Close()
			_ = out.Close()
		}
		_ = zr.Close()
		if !ok {
			fmt.Println("  解压失败")
			_ = os.RemoveAll(ffmpegLocalDir)
			if askYN("重试下载？") {
				continue
			}
			return
		}
		fmt.Printf("→ ffmpeg 就绪 (ffmpeg_local/)  重试 %d 次\n", attempt-1)
		return
	}
}
