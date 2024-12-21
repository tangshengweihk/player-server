package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/gorilla/websocket"
)

type Config struct {
	StreamSource string `json:"streamSource"`
}

var config Config

func init() {
	// 读取配置文件
	data, err := os.ReadFile("config.json")
	if err != nil {
		// 如果配置文件不存在，使用默认值
		config = Config{
			StreamSource: "rtsp://localhost:554/live",
		}
		// 创建默认配置文件
		saveConfig()
		return
	}

	// 解析配置文件
	if err := json.Unmarshal(data, &config); err != nil {
		log.Fatal("配置文件格式错误:", err)
	}
}

func saveConfig() {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		log.Fatal("保存配置失败:", err)
	}
	if err := os.WriteFile("config.json", data, 0644); err != nil {
		log.Fatal("写入配置文件失败:", err)
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// 广播器结构
type Broadcaster struct {
	clients    map[*websocket.Conn]bool
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	broadcast  chan []byte
	mutex      sync.RWMutex
}

// 创建新的广播器
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients:    make(map[*websocket.Conn]bool),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		broadcast:  make(chan []byte, 4096),
	}
}

// 运行广播器
func (b *Broadcaster) Run() {
	for {
		select {
		case client := <-b.register:
			b.mutex.Lock()
			b.clients[client] = true
			clientCount := len(b.clients)
			b.mutex.Unlock()
			log.Printf("客户端已连接. 当前总连接数: %d", clientCount)

		case client := <-b.unregister:
			b.mutex.Lock()
			delete(b.clients, client)
			clientCount := len(b.clients)
			b.mutex.Unlock()
			log.Printf("客户端已断开. 当前总连接数: %d", clientCount)

		case message := <-b.broadcast:
			b.mutex.RLock()
			for client := range b.clients {
				if err := client.WriteMessage(websocket.BinaryMessage, message); err != nil {
					log.Printf("向客户端发送数据失败: %v", err)
					client.Close()
					delete(b.clients, client)
				}
			}
			b.mutex.RUnlock()
		}
	}
}

// 启动 FFmpeg
func (b *Broadcaster) StartFFmpeg() {
	// 确保 HLS 目录存在
	os.MkdirAll("./web/hls", 0755)

	// 启动两个 FFmpeg 进程：一个用于 WebSocket，一个用于 HLS
	go b.startWebSocketFFmpeg()
	go b.startHLSFFmpeg()
}

// WebSocket 流处理
func (b *Broadcaster) startWebSocketFFmpeg() {
	cmd := exec.Command("ffmpeg",
		"-i", config.StreamSource,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-qp", "23",
		"-vf", "scale=1920:1080",
		"-r", "59.94",
		"-maxrate", "10000k",
		"-bufsize", "20000k",
		"-f", "mpegts",
		"-mpegts_flags", "latm",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("WebSocket FFmpeg 错误: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("WebSocket FFmpeg 启动失败: %v", err)
		return
	}

	// 读取并广播
	buf := make([]byte, 188*1024)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			b.broadcast <- data
		}
	}

	cmd.Process.Kill()
}

// HLS 流处理
func (b *Broadcaster) startHLSFFmpeg() {
	cmd := exec.Command("ffmpeg",
		"-i", config.StreamSource,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-qp", "23",
		"-vf", "scale=1920:1080",
		"-r", "59.94",
		"-maxrate", "10000k",
		"-bufsize", "20000k",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "3",
		"-hls_flags", "delete_segments+append_list",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", "./web/hls/stream_%d.ts",
		"./web/hls/stream.m3u8")

	if err := cmd.Start(); err != nil {
		log.Printf("HLS FFmpeg 启动失败: %v", err)
		return
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("HLS FFmpeg 退出: %v", err)
	}
}

var broadcaster *Broadcaster

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// 使用更简单的 MPEG-TS 格式
	cmd := exec.Command("ffmpeg",
		"-i", config.StreamSource,
		"-c:v", "copy", // 直接复制，不重新编码
		"-f", "mpegts", // 使用 MPEG-TS 格式
		"-")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create pipe: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start FFmpeg: %v", err)
		return
	}

	// 读取并发送视频数据
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			log.Printf("Failed to read from FFmpeg: %v", err)
			break
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			log.Printf("Failed to write to WebSocket: %v", err)
			break
		}
	}

	cmd.Process.Kill()
}

func main() {
	log.Printf("启动 RTSP 到 WebSocket/HLS 转换服务")

	// 创建 HLS 目录
	os.MkdirAll("./web/hls", 0755)

	// 获取本机 IP 地址并打印
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		log.Printf("可用网络接口:")
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				log.Printf("- %v", ipnet)
			}
		}
	}

	broadcaster = NewBroadcaster()
	go broadcaster.Run()
	go broadcaster.StartFFmpeg()

	// WebSocket 处理
	http.HandleFunc("/ws", handleWebSocket)

	// HLS 处理
	http.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.StripPrefix("/hls/", http.FileServer(http.Dir("./web/hls"))).ServeHTTP(w, r)
	})

	// 静态文件处理
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		http.FileServer(http.Dir("./web")).ServeHTTP(w, r)
	})

	log.Printf("服务器启动在 http://localhost:8082")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
