package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DGHeroin/relay/webui/model"
	"github.com/DGHeroin/relay/webui/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// 登录失败记录
type loginAttempt struct {
	count     int
	lastTry   time.Time
	lockedUntil time.Time
}

var (
	loginAttempts sync.Map // IP -> *loginAttempt
	maxLoginAttempts = 5
	lockDuration     = 15 * time.Minute
)

const sessionTTL = 24 * time.Hour // 会话有效期 24 小时

// Handlers API处理器
type Handlers struct {
	relayMgr *service.RelayManager
	geoIP    *service.GeoIPService
	wsHub    *WSHub
}

// NewHandlers 创建处理器
func NewHandlers() *Handlers {
	h := &Handlers{
		relayMgr: service.NewRelayManager(),
		geoIP:    service.NewGeoIPService(),
		wsHub:    NewWSHub(),
	}
	go h.wsHub.Run()
	go h.cleanupSessions() // 启动会话清理
	return h
}

// cleanupSessions 定期清理过期会话
func (h *Handlers) cleanupSessions() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if err := model.CleanExpiredSessions(); err != nil {
			log.Printf("清理过期会话失败: %v", err)
		}
	}
}

// Handle 处理 API 请求
func (h *Handlers) Handle(action string, data map[string]interface{}, c *gin.Context) APIResponse {
	parts := strings.SplitN(action, ".", 2)
	if len(parts) != 2 {
		return Error(400, "无效的 action 格式")
	}

	module, method := parts[0], parts[1]

	// 无需认证的接口
	noAuthRequired := map[string]bool{
		"setup.status":   true,
		"setup.init":     true,
		"system.login":   true,
		"system.version": true,
	}

	// 检查是否需要认证
	if !noAuthRequired[action] {
		token := c.GetHeader("Authorization")
		if token == "" {
			return Error(401, "未登录")
		}
		if _, err := model.GetSession(token); err != nil {
			return Error(401, "登录已过期")
		}
	}

	switch module {
	case "setup":
		return h.handleSetup(method, data)
	case "system":
		return h.handleSystem(method, data, c)
	case "relay":
		return h.handleRelay(method, data)
	case "stats":
		return h.handleStats(method, data)
	default:
		return Error(400, "未知模块")
	}
}

// ==================== Setup 模块 ====================

func (h *Handlers) handleSetup(method string, data map[string]interface{}) APIResponse {
	switch method {
	case "status":
		return Success(map[string]interface{}{
			"need_setup": !model.IsSetupCompleted(),
		})

	case "init":
		if model.IsSetupCompleted() {
			return Error(400, "系统已初始化")
		}

		password, _ := data["password"].(string)
		if password == "" {
			return Error(400, "密码不能为空")
		}

		// 加密密码
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return Error(500, "密码加密失败")
		}

		if err := model.SetSetting("admin_password", string(hash)); err != nil {
			return Error(500, "保存密码失败")
		}

		// 设置默认配置
		model.SetSetting("geoip_enabled", "false")
		model.SetSetting("auto_start", "true")

		if err := model.SetSetupCompleted(); err != nil {
			return Error(500, "初始化失败")
		}

		return Success(nil)

	default:
		return Error(400, "未知方法")
	}
}

// ==================== System 模块 ====================

func (h *Handlers) handleSystem(method string, data map[string]interface{}, c *gin.Context) APIResponse {
	switch method {
	case "login":
		clientIP := c.ClientIP()

		// 检查是否被锁定
		if v, ok := loginAttempts.Load(clientIP); ok {
			attempt := v.(*loginAttempt)
			if time.Now().Before(attempt.lockedUntil) {
				remaining := int(time.Until(attempt.lockedUntil).Minutes()) + 1
				return Error(429, fmt.Sprintf("登录已被锁定，请 %d 分钟后再试", remaining))
			}
			// 锁定已过期，重置计数
			if attempt.count >= maxLoginAttempts {
				attempt.count = 0
			}
		}

		password, _ := data["password"].(string)
		storedHash, err := model.GetSetting("admin_password")
		if err != nil {
			return Error(500, "获取密码失败")
		}

		if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)); err != nil {
			// 记录失败尝试
			v, _ := loginAttempts.LoadOrStore(clientIP, &loginAttempt{})
			attempt := v.(*loginAttempt)
			attempt.count++
			attempt.lastTry = time.Now()

			if attempt.count >= maxLoginAttempts {
				attempt.lockedUntil = time.Now().Add(lockDuration)
				return Error(429, fmt.Sprintf("登录失败次数过多，已锁定 %d 分钟", int(lockDuration.Minutes())))
			}

			remaining := maxLoginAttempts - attempt.count
			return Error(401, fmt.Sprintf("密码错误，还剩 %d 次尝试机会", remaining))
		}

		// 登录成功，清除失败记录
		loginAttempts.Delete(clientIP)

		// 生成 token 并存储会话数据
		token, err := generateToken()
		if err != nil {
			return Error(500, "生成令牌失败")
		}
		if err := model.CreateSession(token, sessionTTL); err != nil {
			return Error(500, "创建会话失败")
		}

		return Success(map[string]interface{}{"token": token})

	case "logout":
		token := c.GetHeader("Authorization")
		model.DeleteSession(token)
		return Success(nil)

	case "version":
		return Success(map[string]interface{}{
			"version":    Version,
			"build_time": BuildTime,
			"git_commit": GitCommit,
		})

	case "get_settings":
		settings, err := model.GetAllSettings()
		if err != nil {
			return Error(500, "获取设置失败")
		}
		// 移除敏感信息
		delete(settings, "admin_password")
		return Success(settings)

	case "update_settings":
		key, _ := data["key"].(string)
		value, _ := data["value"].(string)
		if key == "" {
			return Error(400, "key 不能为空")
		}
		// 禁止修改敏感设置
		if key == "admin_password" || key == "setup_completed" {
			return Error(403, "禁止修改此设置")
		}
		if err := model.SetSetting(key, value); err != nil {
			return Error(500, "保存失败")
		}

		// 如果修改了 geoip_enabled，重新加载
		if key == "geoip_enabled" {
			if value == "true" {
				if err := h.geoIP.Load(filepath.Join(dataDir, "GeoLite2-City.mmdb")); err != nil {
					log.Printf("GeoIP 加载失败: %v", err)
					model.SetSetting("geoip_enabled", "false")
					return Error(500, "GeoIP 数据库加载失败")
				}
			} else {
				h.geoIP.Close()
			}
		}

		return Success(nil)

	case "change_password":
		oldPass, _ := data["old_password"].(string)
		newPass, _ := data["new_password"].(string)

		// 后端验证密码长度
		if len(newPass) < 6 {
			return Error(400, "新密码长度至少 6 位")
		}

		storedHash, err := model.GetSetting("admin_password")
		if err != nil {
			return Error(500, "获取密码失败")
		}

		if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(oldPass)); err != nil {
			return Error(401, "原密码错误")
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
		if err != nil {
			return Error(500, "密码加密失败")
		}

		if err := model.SetSetting("admin_password", string(hash)); err != nil {
			return Error(500, "保存密码失败")
		}

		// 清除所有会话，强制重新登录
		model.DeleteAllSessions()

		return Success(nil)

	case "geoip_status":
		return Success(map[string]interface{}{
			"enabled": h.geoIP.IsLoaded(),
			"path":    filepath.Join(dataDir, "GeoLite2-City.mmdb"),
		})

	case "delete_geoip":
		h.geoIP.Close()
		os.Remove(filepath.Join(dataDir, "GeoLite2-City.mmdb"))
		model.SetSetting("geoip_enabled", "false")
		return Success(nil)

	default:
		return Error(400, "未知方法")
	}
}

// HandleGeoIPUpload 处理 GeoIP 文件上传
func (h *Handlers) HandleGeoIPUpload(c *gin.Context) {
	// 验证登录状态
	token := c.GetHeader("Authorization")
	if token == "" {
		c.JSON(200, Error(401, "未登录"))
		return
	}
	if _, err := model.GetSession(token); err != nil {
		c.JSON(200, Error(401, "登录已过期"))
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(200, Error(400, "文件上传失败"))
		return
	}

	dst := filepath.Join(dataDir, "GeoLite2-City.mmdb")
	if err := c.SaveUploadedFile(file, dst); err != nil {
		c.JSON(200, Error(500, "保存文件失败"))
		return
	}

	// 尝试加载
	if err := h.geoIP.Load(dst); err != nil {
		os.Remove(dst)
		c.JSON(200, Error(400, "无效的 GeoIP 数据库文件"))
		return
	}

	model.SetSetting("geoip_enabled", "true")
	c.JSON(200, Success(nil))
}

// ==================== Relay 模块 ====================

func (h *Handlers) handleRelay(method string, data map[string]interface{}) APIResponse {
	switch method {
	case "list":
		rules, err := model.GetAllRelayRules()
		if err != nil {
			return Error(500, "获取规则失败")
		}
		// 附加运行状态
		result := make([]map[string]interface{}, len(rules))
		for i, rule := range rules {
			status := h.relayMgr.GetStatus(rule.ID)
			result[i] = map[string]interface{}{
				"id":          rule.ID,
				"name":        rule.Name,
				"src":         rule.Src,
				"dst":         rule.Dst,
				"protocol":    rule.Protocol,
				"enabled":     rule.Enabled,
				"running":     status.Running,
				"connections": status.Connections,
				"bytes_in":    status.BytesIn,
				"bytes_out":   status.BytesOut,
				"created_at":  rule.CreatedAt,
			}
		}
		return Success(result)

	case "create":
		name, _ := data["name"].(string)
		src, _ := data["src"].(string)
		dst, _ := data["dst"].(string)
		protocol, _ := data["protocol"].(string)
		if protocol == "" {
			protocol = "both"
		}

		log.Printf("[Relay] 创建规则请求: name=%s, src=%s, dst=%s, protocol=%s", name, src, dst, protocol)

		if name == "" || src == "" || dst == "" {
			log.Printf("[Relay] 创建失败: 参数不完整")
			return Error(400, "参数不完整")
		}

		// 验证监听地址格式
		if err := validateListenAddr(src); err != nil {
			log.Printf("[Relay] 创建失败: %v", err)
			return Error(400, err.Error())
		}

		// 验证目标地址格式
		if err := validateTargetAddr(dst); err != nil {
			log.Printf("[Relay] 创建失败: %v", err)
			return Error(400, err.Error())
		}

		// 验证协议
		if protocol != "tcp" && protocol != "udp" && protocol != "both" {
			return Error(400, "协议必须是 tcp、udp 或 both")
		}

		rule, err := model.CreateRelayRule(name, src, dst, protocol)
		if err != nil {
			log.Printf("[Relay] 创建失败: %v", err)
			return Error(500, "创建失败")
		}
		log.Printf("[Relay] 创建成功: id=%s", rule.ID)
		return Success(rule)

	case "update":
		id, _ := data["id"].(string)
		name, _ := data["name"].(string)
		src, _ := data["src"].(string)
		dst, _ := data["dst"].(string)
		protocol, _ := data["protocol"].(string)

		if id == "" {
			return Error(400, "id 不能为空")
		}

		// 验证监听地址格式
		if src != "" {
			if err := validateListenAddr(src); err != nil {
				return Error(400, err.Error())
			}
		}

		// 验证目标地址格式
		if dst != "" {
			if err := validateTargetAddr(dst); err != nil {
				return Error(400, err.Error())
			}
		}

		// 验证协议
		if protocol != "" && protocol != "tcp" && protocol != "udp" && protocol != "both" {
			return Error(400, "协议必须是 tcp、udp 或 both")
		}

		// 如果正在运行，先停止
		if h.relayMgr.IsRunning(id) {
			h.relayMgr.Stop(id)
		}

		if err := model.UpdateRelayRule(id, name, src, dst, protocol); err != nil {
			return Error(500, "更新失败")
		}
		return Success(nil)

	case "delete":
		id, _ := data["id"].(string)
		if id == "" {
			return Error(400, "id 不能为空")
		}

		// 停止运行
		h.relayMgr.Stop(id)

		if err := model.DeleteRelayRule(id); err != nil {
			return Error(500, "删除失败")
		}
		return Success(nil)

	case "start":
		id, _ := data["id"].(string)
		log.Printf("[Relay] 启动请求: id=%s", id)
		if id == "" {
			return Error(400, "id 不能为空")
		}

		rule, err := model.GetRelayRule(id)
		if err != nil {
			log.Printf("[Relay] 启动失败: 规则不存在 id=%s, err=%v", id, err)
			return Error(404, "规则不存在")
		}

		log.Printf("[Relay] 找到规则: name=%s, src=%s, dst=%s, protocol=%s", rule.Name, rule.Src, rule.Dst, rule.Protocol)

		if err := h.relayMgr.Start(rule, h.wsHub, h.geoIP); err != nil {
			log.Printf("[Relay] 启动失败: %v", err)
			return Error(500, err.Error())
		}
		log.Printf("[Relay] 启动成功: %s", rule.Name)
		return Success(nil)

	case "stop":
		id, _ := data["id"].(string)
		log.Printf("[Relay] 停止请求: id=%s", id)
		if id == "" {
			return Error(400, "id 不能为空")
		}
		h.relayMgr.Stop(id)
		log.Printf("[Relay] 停止成功: id=%s", id)
		return Success(nil)

	case "start_all":
		rules, err := model.GetEnabledRelayRules()
		if err != nil {
			return Error(500, "获取规则失败")
		}
		for _, rule := range rules {
			h.relayMgr.Start(rule, h.wsHub, h.geoIP)
		}
		return Success(nil)

	case "stop_all":
		h.relayMgr.StopAll()
		return Success(nil)

	case "set_enabled":
		id, _ := data["id"].(string)
		enabled, _ := data["enabled"].(bool)
		if id == "" {
			return Error(400, "id 不能为空")
		}
		// 如果禁用且正在运行，先停止
		if !enabled && h.relayMgr.IsRunning(id) {
			h.relayMgr.Stop(id)
		}
		if err := model.SetRelayEnabled(id, enabled); err != nil {
			return Error(500, "设置失败")
		}
		return Success(nil)

	case "status":
		id, _ := data["id"].(string)
		if id != "" {
			status := h.relayMgr.GetStatus(id)
			return Success(status)
		}
		// 返回所有状态
		return Success(h.relayMgr.GetAllStatus())

	case "export":
		rules, err := model.GetAllRelayRules()
		if err != nil {
			return Error(500, "获取规则失败")
		}
		return Success(rules)

	case "import":
		rulesData, ok := data["rules"].([]interface{})
		if !ok {
			return Error(400, "无效的规则数据")
		}

		var created, skipped int
		for _, r := range rulesData {
			rule, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := rule["name"].(string)
			src, _ := rule["src"].(string)
			dst, _ := rule["dst"].(string)
			protocol, _ := rule["protocol"].(string)
			if protocol == "" {
				protocol = "both"
			}

			// 检查监听地址是否已存在
			if existing, _ := model.GetRelayRuleBySrc(src); existing != nil {
				skipped++
				continue
			}

			if _, err := model.CreateRelayRule(name, src, dst, protocol); err == nil {
				created++
			}
		}
		return Success(map[string]interface{}{
			"created": created,
			"skipped": skipped,
		})

	default:
		return Error(400, "未知方法")
	}
}

// ==================== Stats 模块 ====================

func (h *Handlers) handleStats(method string, data map[string]interface{}) APIResponse {
	switch method {
	case "overview":
		bytesIn, bytesOut, connections, err := model.GetOverviewStats()
		if err != nil {
			return Error(500, "获取统计失败")
		}
		return Success(map[string]interface{}{
			"total_bytes_in":    bytesIn,
			"total_bytes_out":   bytesOut,
			"total_connections": connections,
			"active_relays":     h.relayMgr.ActiveCount(),
		})

	case "relay":
		id, _ := data["id"].(string)
		rangeStr, _ := data["range"].(string)

		hours := 24
		switch rangeStr {
		case "7d":
			hours = 24 * 7
		case "30d":
			hours = 24 * 30
		}

		stats, err := model.GetRelayStats(id, hours)
		if err != nil {
			return Error(500, "获取统计失败")
		}
		return Success(stats)

	case "logs":
		relayID, _ := data["relay_id"].(string)
		page := int(getFloat(data, "page", 1))
		size := int(getFloat(data, "size", 20))

		logs, total, err := model.GetAccessLogs(relayID, page, size)
		if err != nil {
			return Error(500, "获取日志失败")
		}
		return Success(map[string]interface{}{
			"list":  logs,
			"total": total,
			"page":  page,
			"size":  size,
		})

	case "clear":
		relayID, _ := data["relay_id"].(string)
		if err := model.ClearStats(relayID); err != nil {
			return Error(500, "清除失败")
		}
		return Success(nil)

	default:
		return Error(400, "未知方法")
	}
}

// ==================== WebSocket ====================

// createUpgrader 创建 WebSocket upgrader，验证 Origin
func createUpgrader(r *http.Request) websocket.Upgrader {
	return websocket.Upgrader{
		CheckOrigin: func(req *http.Request) bool {
			origin := req.Header.Get("Origin")
			if origin == "" {
				return true // 允许非浏览器客户端
			}

			// 获取允许的来源
			allowOrigin := getCachedCORSOrigin()
			if allowOrigin == "*" {
				return true
			}

			// 空字符串表示同源策略，检查 Origin 是否与 Host 匹配
			if allowOrigin == "" {
				// 解析 Origin URL
				originURL, err := url.Parse(origin)
				if err != nil {
					return false
				}
				// 比较 host（忽略协议）
				return originURL.Host == req.Host
			}

			// 检查是否在允许列表中
			origins := strings.Split(allowOrigin, ",")
			for _, o := range origins {
				if strings.TrimSpace(o) == origin {
					return true
				}
			}
			return false
		},
	}
}

func (h *Handlers) HandleWebSocket(c *gin.Context) {
	// 验证 token
	token := c.Query("token")
	if token == "" {
		c.JSON(401, Error(401, "未提供认证令牌"))
		return
	}

	if _, err := model.GetSession(token); err != nil {
		c.JSON(401, Error(401, "认证已过期"))
		return
	}

	// 创建 upgrader 并升级连接
	upgrader := createUpgrader(c.Request)
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}

	client := &WSClient{
		hub:    h.wsHub,
		conn:   conn,
		send:   make(chan []byte, 256),
		topics: make(map[string]bool),
	}

	h.wsHub.register <- client

	go client.writePump()
	go client.readPump()
}

// ==================== 工具函数 ====================

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func getFloat(data map[string]interface{}, key string, defaultVal float64) float64 {
	if v, ok := data[key].(float64); ok {
		return v
	}
	return defaultVal
}

// validateListenAddr 验证监听地址格式
func validateListenAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("地址格式错误: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("端口格式错误")
	}

	// 端口范围检查（允许 1-65535，但建议使用非特权端口）
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间")
	}

	// 如果指定了主机，验证格式
	if host != "" && host != "0.0.0.0" && host != "::" {
		if ip := net.ParseIP(host); ip == nil {
			return fmt.Errorf("无效的 IP 地址: %s", host)
		}
	}

	return nil
}

// validateTargetAddr 验证目标地址格式
func validateTargetAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("目标地址格式错误: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("目标端口格式错误")
	}

	if port < 1 || port > 65535 {
		return fmt.Errorf("目标端口必须在 1-65535 之间")
	}

	if host == "" {
		return fmt.Errorf("目标主机不能为空")
	}

	// 检查是否为内网地址（可选安全策略）
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			// 允许内网地址，但记录日志
			log.Printf("[安全警告] 目标地址为内网 IP: %s", addr)
		}
	}

	return nil
}

// isPrivateIP 检查是否为内网 IP
func isPrivateIP(ip net.IP) bool {
	private := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}

	for _, cidr := range private {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}
