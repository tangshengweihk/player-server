package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFS embed.FS

type Config struct {
	StreamSource string `json:"streamSource"`
}

var config Config
var ffmpegPath = "ffmpeg" // 默认使用PATH中的ffmpeg

func init() {
	// 读取配置文件
	data, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatal("无法读取配置文件 config.json:", err)
	}

	// 解析配置文件
	if err := json.Unmarshal(data, &config); err != nil {
		log.Fatal("配置文件格式错误:", err)
	}

	// 确定ffmpeg路径
	executable, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(executable)
		path := filepath.Join(dir, "ffmpeg.exe")
		if _, err := os.Stat(path); err == nil {
			ffmpegPath = path
			log.Printf("检测到 ffmpeg.exe 路径为: %s", ffmpegPath)
		} else {
			log.Printf("未在程序同目录下找到 ffmpeg.exe, 将尝试使用系统 PATH 中的 ffmpeg。")
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Client is a middleman between the websocket connection and the broadcaster.
type Client struct {
	broadcaster *Broadcaster
	conn        *websocket.Conn
	send        chan []byte
}

// readPump pumps messages from the websocket connection to the broadcaster.
// It ensures that the websocket connection is cleanly closed when the client disconnects.
func (c *Client) readPump() {
	defer func() {
		c.broadcaster.unregister <- c
		c.conn.Close()
	}()
	for {
		// The Gorilla WebSocket library automatically handles ping/pong and close messages.
		// A read from the connection will return an error on disconnection.
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
}

// writePump pumps messages from the broadcaster to the websocket connection.
func (c *Client) writePump() {
	defer c.conn.Close()
	for message := range c.send {
		if err := c.conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
			log.Printf("向客户端发送数据失败: %v", err)
			return
		}
	}
}

// 广播器结构
type Broadcaster struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
	mutex      sync.RWMutex
}

// 创建新的广播器
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 1024),
	}
}

// 运行广播器
func (b *Broadcaster) Run() {
	for {
		select {
		case client := <-b.register:
			b.mutex.Lock()
			b.clients[client] = true
			b.mutex.Unlock()
			log.Printf("客户端已连接. 当前总连接数: %d", len(b.clients))

		case client := <-b.unregister:
			b.mutex.Lock()
			if _, ok := b.clients[client]; ok {
				delete(b.clients, client)
				close(client.send)
			}
			b.mutex.Unlock()
			log.Printf("客户端已断开. 当前总连接数: %d", len(b.clients))

		case message := <-b.broadcast:
			b.mutex.Lock()
			for client := range b.clients {
				select {
				case client.send <- message:
				default:
					// 如果客户端的发送缓冲区已满，意味着客户端的消费速度跟不上生产速度。
					// 之前的逻辑是立即断开客户端，这对于视频流来说过于激进。
					// 更好的策略是简单地为这个慢的客户端丢弃这一帧数据，允许它后续赶上。
					// 这在客户端上可能表现为一次微小的卡顿，但连接会保持稳定。
					log.Printf("客户端 %s 的缓冲区已满，为该客户端丢弃一帧数据", client.conn.RemoteAddr())
				}
			}
			b.mutex.Unlock()
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
	cmd := exec.Command(ffmpegPath,
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-i", config.StreamSource,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-crf", "20", // 使用 CRF 控制质量，更接近原画
		"-c:a", "copy", // 直接复制音频流
		"-f", "mpegts",
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
	buf := make([]byte, 188*512)
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
	cmd := exec.Command(ffmpegPath,
		"-i", config.StreamSource,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-crf", "20", // 使用 CRF 控制质量，更接近原画
		"-g", "30", // 每30帧插入一个关键帧，约1秒一个
		"-c:a", "copy", // 直接复制音频流
		"-f", "hls",
		"-hls_time", "1",
		"-hls_list_size", "2",
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

	client := &Client{broadcaster: broadcaster, conn: conn, send: make(chan []byte, 256)}
	broadcaster.register <- client

	go client.writePump()
	go client.readPump()
}

func main() {
	log.Printf("启动 RTSP 到 WebSocket/HLS 转换服务")

	// 创建 HLS 目录
	os.MkdirAll("./web/hls", 0755)

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal("静态文件系统错误: ", err)
	}

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

		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
	})

	log.Printf("服务器启动在 http://localhost:8082")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
