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
	ffmpegPath  string
	hasDXGI     bool
	useFFmpeg   bool
	h264Encoder = ""
)

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

func detectH264Encoder() {
	if h264Encoder != "" {
		return
	}
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return
	}
	if strings.Contains(string(out), "libx264") {
		h264Encoder = "libx264"
		log.Printf("H.264: libx264")
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
