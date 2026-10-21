// Command webvnc serves the local desktop to a web browser and injects the
// browser's mouse and keyboard input back into it. On Windows it drives the
// Windows desktop; on Linux it drives an X11 display (see platformName).
//
// The Vue frontend is embedded in the binary, so the build product is a single
// self-contained executable with no external assets and no CGO dependency.
//
// Build:
//
//	cd frontend && npm install && npm run build
//	cd .. && go build .
//
// See README.md for the full build and deployment notes.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
	"webvnc/server"
)

var version = "0.1.0"

// exeName is what the reader should type to run this binary, for the examples
// in the usage text. It differs per platform because the file does.
func exeName() string {
	if runtime.GOOS == "windows" {
		return "webvnc.exe"
	}
	return "./webvnc"
}

// platformName is the desktop this build drives, written as a trailing
// parenthetical so the sentence around it needs no per-platform spacing.
// Saying "Windows" unconditionally was a lie on Linux -- the same kind of lie
// as a banner advertising audio on a platform with no sound backend, and worth
// the same treatment.
func platformName() string {
	if runtime.GOOS == "windows" {
		return "（Windows）"
	}
	return "（Linux / X11）"
}

type options struct {
	addr       string
	password   string
	fps        int
	quality    int
	scale      float64
	maxClients int
	logLevel   string
	logFormat  string

	maxBitrate int64
	priority   server.Priority
	minQuality int
	minFPS     int
	staticSkip bool
	audio      bool
	viewOnly   bool
	videoMode  server.VideoMode

	// TLS is opt-in and both halves are required together: a certificate
	// without its key cannot serve anything, and serving plaintext while one of
	// the two is set would be the opposite of what was asked for.
	tlsCert string
	tlsKey  string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nwebvnc: %v\n", err)
		fmt.Fprintf(os.Stderr, "运行失败的排查方法见 README.md 的“常见问题”一节。\n")
		os.Exit(1)
	}
}

func run() error {
	// Must come before any output: the console starts on the system codepage.
	server.EnableUTF8Console()

	opts := parseFlags()

	logger, err := newLogger(opts.logLevel, opts.logFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	// DPI awareness must be set before anything queries screen metrics.
	server.EnableDPIAwareness()

	password := opts.password
	generated := false
	if password == "" {
		password = server.GeneratePassword()
		generated = true
	}

	auth := server.NewAuthenticator(server.AuthConfig{Password: password})

	hub, err := server.NewHub(server.HubConfig{
		FPS:        opts.fps,
		Quality:    opts.quality,
		Scale:      opts.scale,
		MaxClients: opts.maxClients,

		MaxBitrate: opts.maxBitrate,
		Priority:   opts.priority,
		MinQuality: opts.minQuality,
		MinFPS:     opts.minFPS,
		StaticSkip: opts.staticSkip,
		VideoMode:  opts.videoMode,
		ViewOnly:   opts.viewOnly,

		// -audio defaults to on, but a platform with no audio backend cannot
		// honour it. Turning the route off there is the honest answer: the
		// client's optional-stream logic is built for exactly this case ("a
		// server with the feature switched off refuses the endpoint every
		// time"), so the button reports a refusal instead of retrying into a
		// silence that looks like a hang.
		AudioEnabled: opts.audio && server.AudioSupported(),
	}, auth, logger)
	if err != nil {
		return fmt.Errorf("初始化屏幕捕获失败: %w", err)
	}
	defer hub.Close()

	assetFS, err := frontendFS()
	if err != nil {
		return fmt.Errorf("读取内嵌前端资源失败: %w", err)
	}
	if _, err := fs.Stat(assetFS, "index.html"); err != nil {
		return errors.New("内嵌前端资源缺少 index.html，请先在 frontend/ 目录执行 npm run build")
	}

	// TLS is decided before the banner, because the banner has to advertise the
	// scheme the user will actually type. A bad certificate stops startup here
	// rather than at the first connection: a server that silently fell back to
	// plaintext would leak the password over the wire.
	leaf, err := loadTLS(opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              opts.addr,
		Handler:           logRequests(logger, newMux(hub, auth, logger, assetFS)),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off long-lived WebSocket streams.
		IdleTimeout: 120 * time.Second,
	}

	printBanner(logger, srv.Addr, hub, opts, generated, password, leaf)

	// Started after the banner so the first log line does not land in the
	// middle of it.
	go hub.Run(ctx)
	// Audio is a second, independent loop: it has its own capture cadence and
	// its own socket, so neither stream can stall the other.
	go hub.RunAudio(ctx)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if leaf != nil {
			err = srv.ListenAndServeTLS(opts.tlsCert, opts.tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("监听 %s 失败: %w", srv.Addr, err)
	case <-ctx.Done():
		logger.Info("正在关闭服务…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func parseFlags() options {
	var opts options

	flag.StringVar(&opts.addr, "addr", ":8339", "监听地址，例如 :8339 或 127.0.0.1:9000")
	flag.IntVar(&opts.fps, "fps", 30, "屏幕推送帧率")
	flag.IntVar(&opts.quality, "quality", 65, "JPEG 编码质量 (1-100)")
	flag.Float64Var(&opts.scale, "scale", 1.0, "画面缩放比例 (0,1]，降低可显著节省带宽和 CPU")
	flag.StringVar(&opts.password, "password", "", "访问口令；留空则每次启动随机生成并打印在日志里")
	flag.IntVar(&opts.maxClients, "max-clients", 8, "允许同时连接的客户端数量上限，0 表示不限制")
	flag.StringVar(&opts.logLevel, "log-level", "info", "日志级别: debug | info | warn | error")
	flag.StringVar(&opts.logFormat, "log-format", "text", "日志格式: text | json")

	var maxBitrate string
	flag.StringVar(&maxBitrate, "max-bitrate", "0",
		"服务端总出口码率上限，0 表示不限；支持 800k / 2M，或纯数字（单位 kbps）")
	var videoCodec string
	var priority string
	flag.StringVar(&priority, "priority", "fps",
		"超预算时的取舍: fps=先降画质保帧率 | quality=先降帧率保画质")
	flag.IntVar(&opts.minQuality, "min-quality", 30, "码率控制可降到的最低 JPEG 质量")
	flag.IntVar(&opts.minFPS, "min-fps", 5, "码率控制可降到的最低帧率")
	flag.BoolVar(&opts.staticSkip, "static-skip", true,
		"画面无变化时跳过编码与发送；排查问题时可用 -static-skip=false 关闭")
	flag.StringVar(&opts.tlsCert, "tls-cert", "",
		"TLS 证书链文件（PEM）；与 -tls-key 同时给出时改用 https / wss 提供服务")
	flag.StringVar(&opts.tlsKey, "tls-key", "",
		"TLS 私钥文件（PEM），与 -tls-cert 配对")
	// The flag exists on every platform so a shared command line keeps working,
	// but only a platform with a backend can honour it -- so only that platform
	// gets told it captures the system loopback.
	audioUsage := "声音传输；本平台没有声音采集实现，开启也不会有声音（见 README 平台支持）"
	if server.AudioSupported() {
		audioUsage = "开放声音传输（系统回环采集）；客户端默认静音，点“开启声音”后才有声。用 -audio=false 完全关闭"
	}
	flag.BoolVar(&opts.audio, "audio", true, audioUsage)
	flag.BoolVar(&opts.viewOnly, "view-only", false,
		"直播模式：客户端只能看画面、听声音，不能操作这台电脑。开关在服务端，"+
			"客户端无法解除——改前端或直接发 WebSocket 帧也绕不过")
	flag.StringVar(&videoCodec, "codec", "auto",
		"画面压缩方式: auto=优先帧间(H.264)压缩，不可用时自动退回帧内(JPEG) | h264=只用帧间 | jpeg=只用帧内")

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "webvnc %s - 在浏览器里远程查看并控制这台电脑%s\n\n", version, platformName())
		fmt.Fprintf(out, "用法: %s [参数]\n\n参数:\n", os.Args[0])
		flag.PrintDefaults()
		// Not a format string: the examples contain a literal "75%", and
		// spelling that "75%%" in an Fprintf would put the escaping in the
		// reader's way for no reason. The executable name goes in by hand.
		fmt.Fprint(out, strings.ReplaceAll(`
示例:
  @EXE@                                   # 监听 8339，随机口令
  @EXE@ -password mySecret -fps 20        # 指定口令和帧率
  @EXE@ -addr 127.0.0.1:9000 -scale 0.75  # 只监听本机，画面缩放到 75%
  @EXE@ -max-bitrate 2M                   # 总出口限制在 2 Mbps，自动降级
  @EXE@ -max-bitrate 2M -priority quality # 同样限额，但优先保住画质
  @EXE@ -audio=false                      # 完全不采集、不传输声音
  @EXE@ -view-only                        # 直播模式：别人只能看和听，不能操作
  @EXE@ -tls-cert cert.pem -tls-key key.pem  # 用证书以 https / wss 提供服务

浏览器访问 http://<本机IP>:8339，输入口令即可；带 -tls-cert 时改用 https://。
`, "@EXE@", exeName()))
	}

	flag.Parse()

	if opts.scale <= 0 {
		opts.scale = 1
	}
	if opts.scale > 1 {
		fmt.Fprintf(os.Stderr, "提示: -scale 不能大于 1，已按 1.0 处理\n")
		opts.scale = 1
	}
	if opts.fps <= 0 {
		opts.fps = 30
	}
	if opts.fps > 60 {
		fmt.Fprintf(os.Stderr, "提示: -fps 上限为 60，已按 60 处理\n")
		opts.fps = 60
	}
	if opts.quality < 1 || opts.quality > 100 {
		fmt.Fprintf(os.Stderr, "提示: -quality 需在 1-100 之间，已按 65 处理\n")
		opts.quality = 65
	}

	// The bitrate flag is parsed here rather than by flag.Value so the error can
	// point at the accepted spellings instead of just "invalid value".
	bitrate, err := server.ParseBitrate(maxBitrate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "提示: -max-bitrate %v，已按 0（不限）处理\n", err)
		bitrate = 0
	}
	opts.maxBitrate = bitrate

	parsedPriority, err := server.ParsePriority(priority)
	if err != nil {
		fmt.Fprintf(os.Stderr, "提示: %v，已按 fps 处理\n", err)
		parsedPriority = server.PriorityFPS
	}
	opts.priority = parsedPriority

	parsedCodec, err := server.ParseVideoMode(videoCodec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "提示: %v，已按 auto 处理\n", err)
		parsedCodec = server.ModeAuto
	}
	opts.videoMode = parsedCodec

	if opts.minQuality < 1 || opts.minQuality > opts.quality {
		fmt.Fprintf(os.Stderr, "提示: -min-quality 需在 1-%d 之间，已按 %d 处理\n", opts.quality, opts.quality)
		opts.minQuality = opts.quality
	}
	if opts.minFPS < 1 || opts.minFPS > opts.fps {
		fmt.Fprintf(os.Stderr, "提示: -min-fps 需在 1-%d 之间，已按 %d 处理\n", opts.fps, opts.fps)
		opts.minFPS = opts.fps
	}
	return opts
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info", "":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("未知的日志级别 %q", level)
	}

	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text", "":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("未知的日志格式 %q", format)
	}
}

// newMux wires up every route the server exposes. Split out from run() so tests
// exercise the same wiring that ships.
func newMux(hub *server.Hub, auth *server.Authenticator, logger *slog.Logger, assetFS fs.FS) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", loginHandler(auth, logger))
	mux.HandleFunc("/api/session", sessionHandler(auth))
	mux.HandleFunc("/api/stats", statsHandler(hub))
	mux.HandleFunc("/api/verify", verifyHandler(auth))
	mux.HandleFunc("/ws", hub.ServeWS)
	// Registered unconditionally: with -audio=false the handler answers 404,
	// which is a clearer answer to "is audio on?" than a routing miss.
	mux.HandleFunc("/ws/audio", hub.ServeAudioWS)
	mux.Handle("/", spaHandler(assetFS))
	return mux
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
}

func loginHandler(auth *server.Authenticator, logger *slog.Logger) http.HandlerFunc {
	type fail struct {
		Error     string `json:"error"`
		RetryInMS int64  `json:"retryInMs,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req loginRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, fail{Error: "请求格式错误"})
			return
		}

		ip := clientIP(r)
		token, expires, locked, ok := auth.Login(ip, req.Password)
		if !ok {
			if locked > 0 {
				logger.Warn("login locked out", "remote", ip, "retry_in", locked.Round(time.Second))
				w.Header().Set("Retry-After", fmt.Sprint(int(locked.Seconds())))
				writeJSON(w, http.StatusTooManyRequests, fail{
					Error:     fmt.Sprintf("口令错误次数过多，请 %d 秒后重试", int(locked.Seconds())+1),
					RetryInMS: locked.Milliseconds(),
				})
				return
			}
			logger.Warn("login failed", "remote", ip)
			writeJSON(w, http.StatusUnauthorized, fail{Error: "口令错误"})
			return
		}

		logger.Info("login ok", "remote", ip)
		writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: expires.UnixMilli()})
	}
}

func sessionHandler(auth *server.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"authRequired": auth.Enabled(),
			"version":      version,
		})
	}
}

// verifyHandler lets the frontend tell "the token expired" apart from "the
// network dropped": a failed WebSocket handshake gives the browser no status
// code, so it cannot distinguish the two on its own.
func verifyHandler(auth *server.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.Validate(r.URL.Query().Get("token")) {
			writeJSON(w, http.StatusOK, map[string]any{"valid": true})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"valid": false})
	}
}

func statsHandler(hub *server.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		width, height := hub.ScreenSize()
		writeJSON(w, http.StatusOK, map[string]any{
			"clients":    hub.Count(),
			"fps":        hub.FPS(),
			"framesSent": hub.FramesSent(),
			"screen":     map[string]int{"width": width, "height": height},

			// The live stream settings, after rate control has had its say.
			"quality":   hub.EffectiveQuality(),
			"targetFPS": hub.EffectiveFPS(),
			"kbps":      hub.Bitrate() / 1000,
			"idle":      hub.Idle(),
			"limited":   hub.Limited(),
			"atFloors":  hub.AtFloors(),

			// Which compressor the stream is running, and what it gave up. Under
			// -codec auto this can change while the server is up, so it is a
			// reading rather than a setting.
			"video":       hub.VideoCodec().String(),
			"videoDetail": hub.VideoDescription(),

			// Audio listeners are counted separately from viewers, and the
			// rate is reported on its own so the two can be added up to get
			// the total egress the -max-bitrate ceiling governs.
			"audioClients": hub.AudioClients(),
			"audioKbps":    hub.AudioKbps(),
			"audioIdle":    hub.AudioIdle(),
		})
	}
}

// spaHandler serves the embedded frontend, falling back to index.html so that
// client-side routes survive a hard refresh.
func spaHandler(assetFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}

		if _, err := fs.Stat(assetFS, name); err != nil {
			// Unknown path: hand back the SPA shell.
			serveIndex(w, r, assetFS)
			return
		}

		if strings.HasPrefix(name, "assets/") {
			// Vite fingerprints these filenames, so they are safe to cache hard.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assetFS fs.FS) {
	data, err := fs.ReadFile(assetFS, "index.html")
	if err != nil {
		http.Error(w, "frontend assets are missing from this build", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// logRequests logs the request path only. The WebSocket token travels in the
// query string and must never reach the log file.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		if strings.HasPrefix(r.URL.Path, "/ws") || rec.status >= 400 {
			logger.Debug("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration", time.Since(start).Round(time.Millisecond),
				"remote", clientIP(r))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack lets the WebSocket upgrade take over the raw connection. Without it
// the wrapper would hide the underlying ResponseWriter's Hijacker and the
// upgrade would fail with "response does not implement http.Hijacker".
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	r.wroteHeader = true
	r.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush forwards streaming flushes to the underlying writer.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// Startup banner
// ---------------------------------------------------------------------------

// loadTLS reads the certificate pair named on the command line. A nil leaf means
// "serve plaintext", which is the default. The pair is all-or-nothing: a
// half-configured invocation fails here rather than quietly serving http, which
// would send the password in the clear under a flag that asked for the opposite.
func loadTLS(opts options) (*x509.Certificate, error) {
	if opts.tlsCert == "" && opts.tlsKey == "" {
		return nil, nil
	}
	if opts.tlsCert == "" || opts.tlsKey == "" {
		return nil, errors.New("-tls-cert 与 -tls-key 必须同时给出")
	}
	pair, err := tls.LoadX509KeyPair(opts.tlsCert, opts.tlsKey)
	if err != nil {
		return nil, fmt.Errorf("加载 TLS 证书失败: %w", err)
	}
	// Certificate[0] is the leaf; the chain follows it. Only the leaf matters for
	// the banner -- it is the one the browser matches the hostname against.
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析 TLS 证书失败: %w", err)
	}
	return leaf, nil
}

// certNames lists what a certificate is valid for, for display in the banner.
func certNames(leaf *x509.Certificate) []string {
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses)+1)
	names = append(names, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	// CommonName is only a fallback: browsers have ignored it for hostname
	// matching since RFC 6125, so a certificate carrying only a CN covers
	// nothing in practice -- but it is still the most informative thing to print.
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = append(names, leaf.Subject.CommonName)
	}
	return names
}

// firstHost picks one name to suggest. A certificate with nothing in it at all
// is possible, hence the fallback.
func firstHost(names []string) string {
	if len(names) == 0 {
		return "localhost"
	}
	// A wildcard in a URL is not typeable, so it is shown as the placeholder it
	// is: the server has no way to know which subdomain this machine answers to.
	return strings.Replace(names[0], "*.", "<子域名>.", 1)
}

// coversAny reports whether the certificate validates for at least one of the
// hosts the banner is about to advertise.
func coversAny(leaf *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if leaf.VerifyHostname(h) == nil {
			return true
		}
	}
	return false
}

func printBanner(logger *slog.Logger, addr string, hub *server.Hub, opts options, generated bool, password string, leaf *x509.Certificate) {
	width, height := hub.ScreenSize()
	port, bindHost := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		port, bindHost = p, h
	}

	scheme := "http"
	if leaf != nil {
		scheme = "https"
	}
	// The hosts advertised below, collected so the certificate can be checked
	// against them once they are all known.
	var advertised []string

	fmt.Fprintf(os.Stdout, "\n")
	fmt.Fprintf(os.Stdout, "  远程桌面服务已启动\n", version)
	fmt.Fprintf(os.Stdout, "  ────────────────────────────────────────────\n")
	fmt.Fprintf(os.Stdout, "  监听地址   %s\n", addr)
	fmt.Fprintf(os.Stdout, "  屏幕分辨率 %d x %d\n", width, height)

	// Which of the two encoders this machine will actually run, and what it
	// refused. It is printed rather than logged because it decides how much
	// bandwidth the stream costs, and because every refusal behind it is silent
	// from the outside.
	if desc := hub.VideoDescription(); desc != "" {
		fmt.Fprintf(os.Stdout, "  画面压缩   %s\n", desc)
	}

	if opts.maxBitrate > 0 {
		fmt.Fprintf(os.Stdout, "  码率上限   %s (总出口)  %s优先, 最低 %d 帧 / 质量 %d\n",
			formatBitrate(opts.maxBitrate), opts.priority, opts.minFPS, opts.minQuality)
	} else {
		fmt.Fprintf(os.Stdout, "  码率上限   不限 (可用 -max-bitrate 开启自适应降级)\n")
	}

	// The exact audio rate is the capture device's mix rate, which is not known
	// until a listener connects; 44.1 kHz and 48 kHz stereo bracket it.
	// The banner has to describe what this machine will actually do, not what
	// -audio asked for. A line reading 开启 above a platform that has no capture
	// backend is a claim the operator cannot check from outside: the failure is
	// a button press and four reconnect attempts away from the sentence.
	switch {
	case !opts.audio:
		fmt.Fprintf(os.Stdout, "  声音       关闭  (-audio=false)\n")
	case !server.AudioSupported():
		fmt.Fprintf(os.Stdout, "  声音       不支持  本平台没有声音采集实现（见 README 平台支持）\n")
	default:
		suffix := ""
		if opts.maxBitrate > 0 {
			suffix = "，先占用总出口上限"
		}
		fmt.Fprintf(os.Stdout, "  声音       开启  系统回环采集，未压缩 PCM 约 1.4-1.5 Mbps%s\n", suffix)
	}

	if opts.viewOnly {
		fmt.Fprintf(os.Stdout, "  客户端权限 只读  直播模式：只能看画面、听声音，不能操作（-view-only）\n")
	}

	if generated {
		fmt.Fprintf(os.Stdout, "  访问口令   %s   (本次随机生成)\n", password)
	} else {
		fmt.Fprintf(os.Stdout, "  访问口令   %s\n", maskSecret(password))
	}

	if leaf != nil {
		expiry := leaf.NotAfter.Format("2006-01-02")
		if time.Now().After(leaf.NotAfter) {
			expiry += " 已过期"
		}
		fmt.Fprintf(os.Stdout, "  传输加密   %s  证书 %s，有效期至 %s\n",
			scheme, strings.Join(certNames(leaf), " "), expiry)
	}

	// Which loopback URL is worth printing depends on what the bind covers. An
	// empty host or "::" is a wildcard, and Windows answers both families on it;
	// a specific address only answers on its own family, so advertising the
	// other one would be a URL that cannot work.
	switch {
	case bindHost == "" || bindHost == "::":
		fmt.Fprintf(os.Stdout, "  本机访问   %s://127.0.0.1:%s  ·  %s://[::1]:%s\n", scheme, port, scheme, port)
		advertised = append(advertised, "127.0.0.1", "::1")
	case strings.Contains(bindHost, ":"):
		fmt.Fprintf(os.Stdout, "  本机访问   %s://[%s]:%s\n", scheme, bindHost, port)
		advertised = append(advertised, bindHost)
	default:
		fmt.Fprintf(os.Stdout, "  本机访问   %s://%s:%s\n", scheme, bindHost, port)
		advertised = append(advertised, bindHost)
	}

	// Likewise, the LAN addresses are only reachable when the bind is a
	// wildcard; with a pinned address the loopback line above already is the
	// reachable one.
	globalV6 := false
	if bindHost == "" || bindHost == "::" {
		v4, v6 := lanAddrs()
		for _, ip := range v4 {
			fmt.Fprintf(os.Stdout, "  局域网访问 %s://%s:%s\n", scheme, ip, port)
			advertised = append(advertised, ip)
		}
		for _, ip := range v6 {
			fmt.Fprintf(os.Stdout, "  IPv6 访问  %s://[%s]:%s\n", scheme, ip, port)
			advertised = append(advertised, ip)
			if p := net.ParseIP(ip); p != nil && !p.IsPrivate() {
				globalV6 = true
			}
		}
	}

	// A certificate that covers none of the addresses above is the normal case
	// for a public-domain certificate, and it fails in the browser as a
	// certificate warning -- which reads as "the server is broken" rather than
	// "use the other name". Naming the covered host removes that guesswork.
	if leaf != nil && !coversAny(leaf, advertised) {
		fmt.Fprintf(os.Stdout, "  注意: 证书不覆盖上面的地址，直接用它们打开会报证书错误；\n")
		fmt.Fprintf(os.Stdout, "        请通过证书覆盖的域名访问，例如 https://%s:%s。\n", firstHost(certNames(leaf)), port)
	}

	fmt.Fprintf(os.Stdout, "  ────────────────────────────────────────────\n")
	fmt.Fprintf(os.Stdout, "  安全提示: 请勿直接暴露到公网，建议配合内网 / VPN / 反向代理使用。\n")
	if leaf != nil {
		fmt.Fprintf(os.Stdout, "            证书只保护传输过程，口令仍是唯一的访问控制，请务必使用强口令。\n")
	}
	if globalV6 {
		fmt.Fprintf(os.Stdout, "            上面的 IPv6 地址是公网可路由的：若路由器未拦截入站，\n")
		fmt.Fprintf(os.Stdout, "            外网可以直接访问到本服务，请务必确认口令强度。\n")
	}
	fmt.Fprintf(os.Stdout, "  按 Ctrl+C 退出。\n\n")

	logger.Info("server started", "addr", addr, "resolution", fmt.Sprintf("%dx%d", width, height),
		"view_only", opts.viewOnly)
}

// formatBitrate renders a bits-per-second figure the way an operator would
// write it, so the banner shows "2 Mbps" rather than "2000000".
func formatBitrate(bps int64) string {
	switch {
	case bps >= 1_000_000_000 && bps%1_000_000_000 == 0:
		return fmt.Sprintf("%d Gbps", bps/1_000_000_000)
	case bps >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(bps)/1_000_000), ".0") + " Mbps"
	default:
		return fmt.Sprintf("%d kbps", bps/1000)
	}
}

// maskSecret reveals just enough of a user-supplied password to confirm which
// one is in use without printing the whole secret.
func maskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 2 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:1]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1:])
}

// lanAddrs lists the addresses an operator could open from another device,
// split by family because the two need different URL forms -- an IPv6 literal
// has to be bracketed before a port can follow it.
//
// Loopback and link-local addresses are dropped. Link-local (fe80::/10) is
// deliberately excluded: it is only reachable on the same segment and the URL
// needs a scope zone (`http://[fe80::1%2516]:8339`), which is never what
// someone wants to read off a banner while a usable address exists.
func lanAddrs() (v4, v6 []string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}

	seen := make(map[string]bool)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			// IsGlobalUnicast covers IPv4 private ranges and IPv6 ULA
			// (fc00::/7), which are exactly the addresses an intranet wants.
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
				continue
			}
			// To4 also matches IPv4-mapped IPv6, so those are reported once,
			// as the IPv4 address they are.
			if v4ip := ip.To4(); v4ip != nil {
				if s := v4ip.String(); !seen[s] {
					seen[s] = true
					v4 = append(v4, s)
				}
				continue
			}
			if s := ip.String(); !seen[s] {
				seen[s] = true
				v6 = append(v6, s)
			}
		}
	}
	return v4, v6
}
