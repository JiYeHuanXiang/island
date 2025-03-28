package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/gorilla/websocket"
)

type Config struct {
	HTTPPort  string `env:"HTTP_PORT" envDefault:"8088"`
	QQWSURL   string `env:"QQ_WS_URL" envDefault:"ws://127.0.0.1:3009"`
	LocalWSURL string `env:"LOCAL_WS_URL" envDefault:"ws://127.0.0.1:3005"`
	QQGroupID int64  `env:"QQ_GROUP_ID"`
}

type ConnectionManager struct {
	conn     *websocket.Conn
	url      string
	mu       sync.RWMutex
	retries  int
	maxRetry int
	quit     chan struct{}
}

type OneBotMessage struct {
	PostType    string      `json:"post_type"`
	MessageType string      `json:"message_type"`
	Message     interface{} `json:"message"`
	UserID      int64       `json:"user_id"`
	GroupID     int64       `json:"group_id"`
}

type ResponseMessage struct {
	Action string      `json:"action"`
	Params interface{} `json:"params"`
}

type CommandRequest struct {
	Command string `json:"command"`
}

type CommandResponse struct {
	Response string `json:"response"`
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	webClients    = make(map[*websocket.Conn]struct{})
	webMutex      sync.RWMutex
	cocAttributes = [...]string{"STR", "CON", "SIZ", "DEX", "APP", "INT", "POW", "EDU", "LUK"}
	qqManager     *ConnectionManager
	localManager  *ConnectionManager
	appConfig     *Config

	rollRegex       = regexp.MustCompile(`^r\s*((?:\d*d?\d+[\+\-\*]\d+)+|(?:\d*d\d+(?:[\+\-\*]\d+)*)+|(?:\d+[\+\-\*]\d+)+)$`)
	scRegex         = regexp.MustCompile(`sc\s+(\d+)/(\d+)`)
	raRegex         = regexp.MustCompile(`^ra\s+(\d+)$`)
	rcRegex         = regexp.MustCompile(`^rc\s+(\d+)$`)
	rbRegex         = regexp.MustCompile(`^rb\s+(\d+)$`)
	reasonRollRegex = regexp.MustCompile(`^r(d?)\s*(.*)$`)
	setDiceRegex    = regexp.MustCompile(`^set(\d+)$`)
	enRegex         = regexp.MustCompile(`^en\s+(\d+)$`)
	tiRegex         = regexp.MustCompile(`^ti$`)
	liRegex         = regexp.MustCompile(`^li$`)
	stRegex         = regexp.MustCompile(`^st\s+([^\d]+)\s+(\d+)(?:\s+([^\d]+)\s+(\d+))?(?:\s+([^\d]+)\s+(\d+))?(?:\s+([^\d]+)\s+(\d+))?$`)
	coc7Regex       = regexp.MustCompile(`^coc7$`)

	defaultDiceSides = 100
	diceMutex        sync.RWMutex

	ErrMaxRetries  = errors.New("maximum retry attempts reached")
	ErrInvalidMsg  = errors.New("invalid message format")
	ErrConnClosed  = errors.New("connection closed")
	ErrConfigLoad  = errors.New("configuration load failed")
	ErrInvalidDice = errors.New("invalid dice sides")
)

func NewConnectionManager(url string, maxRetry int) *ConnectionManager {
	return &ConnectionManager{
		url:      url,
		maxRetry: maxRetry,
		quit:     make(chan struct{}),
	}
}

func (cm *ConnectionManager) Connect() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil {
		return nil
	}

	var err error
	for i := 0; i < cm.maxRetry; i++ {
		cm.conn, _, err = websocket.DefaultDialer.Dial(cm.url, nil)
		if err == nil {
			cm.retries = 0
			return nil
		}

		select {
		case <-cm.quit:
			return ErrConnClosed
		default:
			waitTime := time.Duration(i+1) * time.Second
			log.Printf("连接失败 (尝试 %d/%d), %v秒后重试...", i+1, cm.maxRetry, waitTime)
			time.Sleep(waitTime)
		}
	}
	return fmt.Errorf("%w: %v", ErrMaxRetries, err)
}

func (cm *ConnectionManager) Get() (*websocket.Conn, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.conn == nil {
		return nil, ErrConnClosed
	}
	return cm.conn, nil
}

func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	close(cm.quit)
	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}
}

func loadConfig() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigLoad, err)
	}

	if cfg.QQGroupID == 0 {
		return nil, fmt.Errorf("%w: QQ_GROUP_ID is required", ErrConfigLoad)
	}

	return &cfg, nil
}

func main() {
	rand.Seed(time.Now().UnixNano())

	var err error
	appConfig, err = loadConfig()
	if err != nil {
		log.Printf("配置加载错误: %v", err)
		log.Println("使用默认配置继续运行...")
		appConfig = &Config{
			HTTPPort:   "8088",
			QQWSURL:    "ws://127.0.0.1:3009",
			LocalWSURL: "ws://127.0.0.1:3005",
			QQGroupID:  0,
		}
	}

	qqManager = NewConnectionManager(appConfig.QQWSURL, 5)
	localManager = NewConnectionManager(appConfig.LocalWSURL, 5)
	defer qqManager.Close()
	defer localManager.Close()

	if err := qqManager.Connect(); err != nil {
		log.Printf("初始化QQ连接失败: %v", err)
		log.Println("将在消息处理时尝试重新连接...")
	}

	if err := localManager.Connect(); err != nil {
		log.Printf("初始化本地Web UI连接失败: %v", err)
		log.Println("将在消息处理时尝试重新连接...")
	}

	go startHTTPServer()

	messageLoop()
}

func messageLoop() {
	for {
		conn, err := qqManager.Get()
		if err != nil {
			if err := qqManager.Connect(); err != nil {
				log.Printf("QQ连接不可用: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
			conn, _ = qqManager.Get()
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("读取消息错误: %v", err)
			qqManager.mu.Lock()
			qqManager.conn = nil
			qqManager.mu.Unlock()
			time.Sleep(1 * time.Second)
			continue
		}

		msg, err := parseIncomingMessage(message)
		if err != nil {
			log.Printf("消息解析错误: %v", err)
			continue
		}

		if msg.PostType == "message" {
			handleMessage(conn, msg)
		}
	}
}

func parseIncomingMessage(data []byte) (*OneBotMessage, error) {
	var msg OneBotMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMsg, err)
	}
	return &msg, nil
}

func startHTTPServer() {
	http.HandleFunc("/", serveStatic)
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/command", handleCommand)
	log.Printf("Web服务器已启动 :%s", appConfig.HTTPPort)
	if err := http.ListenAndServe(":"+appConfig.HTTPPort, nil); err != nil {
		log.Fatalf("HTTP服务器错误: %v", err)
	}
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Path)
	if path == "/" || path == "/index.html" {
		http.ServeFile(w, r, "UI.html")
		return
	}
	http.ServeFile(w, r, path)
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := processCommand(req.Command)
	json.NewEncoder(w).Encode(CommandResponse{Response: response})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}

	webMutex.Lock()
	webClients[conn] = struct{}{}
	webMutex.Unlock()

	go func() {
		defer func() {
			webMutex.Lock()
			delete(webClients, conn)
			webMutex.Unlock()
			conn.Close()
		}()

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					log.Printf("WebSocket读取错误: %v", err)
				}
				break
			}

			response := processCommand(string(message))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(response)); err != nil {
				log.Printf("WebSocket写入错误: %v", err)
				break
			}

			if err := sendToQQ(response); err != nil {
				log.Printf("发送到QQ失败: %v", err)
			}
		}
	}()
}

func handleMessage(conn *websocket.Conn, msg *OneBotMessage) {
	messageStr, err := extractMessageContent(msg.Message)
	if err != nil {
		log.Printf("消息内容提取失败: %v", err)
		return
	}

	if !strings.HasPrefix(messageStr, ".") {
		return
	}

	response := processCommand(messageStr)
	if err := sendResponse(conn, msg, response); err != nil {
		log.Printf("发送响应失败: %v", err)
		return
	}

	broadcastToWeb(formatWebMessage(msg, response))
}

func extractMessageContent(msg interface{}) (string, error) {
	switch m := msg.(type) {
	case string:
		return m, nil
	case []interface{}:
		var builder strings.Builder
		for _, v := range m {
			seg, ok := v.(map[string]interface{})
			if !ok {
				continue
			}

			if seg["type"] == "text" {
				data, ok := seg["data"].(map[string]interface{})
				if !ok {
					continue
				}

				text, ok := data["text"].(string)
				if ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String(), nil
	default:
		return "", fmt.Errorf("%w: unexpected type %T", ErrInvalidMsg, msg)
	}
}

func formatWebMessage(msg *OneBotMessage, response string) string {
	if msg.MessageType == "group" {
		return fmt.Sprintf("[群消息] %s", response)
	}
	return fmt.Sprintf("[私聊] %s", response)
}

func processCommand(cmd string) string {
	cmd = strings.TrimPrefix(cmd, ".")
	switch {
	case rollRegex.MatchString(cmd):
		return processRoll(cmd)
	case scRegex.MatchString(cmd):
		return processSanCheck(cmd)
	case cmd == "coc7":
		return processCoC7()
	case raRegex.MatchString(cmd):
		return processRACheck(cmd)
	case rcRegex.MatchString(cmd):
		return processRCCheck(cmd)
	case rbRegex.MatchString(cmd):
		return processRBCheck(cmd)
	case reasonRollRegex.MatchString(cmd):
		return processReasonRoll(cmd)
	case setDiceRegex.MatchString(cmd):
		return processSetDice(cmd)
	case enRegex.MatchString(cmd):
		return processEnCheck(cmd)
	case tiRegex.MatchString(cmd):
		return processTICheck()
	case liRegex.MatchString(cmd):
		return processLICheck()
	case stRegex.MatchString(cmd):
		return processStCheck(cmd)
	case cmd == "help":
		return "COC指令帮助:\n" +
			".r[骰子指令] 掷骰\n" +
			".coc7 生成调查员(7版规则)\n" +
			".sc [成功损失]/[失败损失] 理智检定\n" +
			".ra [技能值] COCTRPG检定\n" +
			".rc [技能值] COC7th核心规则检定\n" +
			".rb [技能值] 奖励骰检定\n" +
			".en [技能值] 技能成长检定\n" +
			".ti 临时疯狂症状\n" +
			".li 总结性疯狂症状\n" +
			".st [技能名] [数值] 记录技能属性(可多个)\n" +
			".r[理由] 带理由的投掷\n" +
			".set[数字] 设置默认骰子面数(如.set6)"
	default:
		return "未知指令，请输入.help查看帮助"
	}
}

func processSetDice(cmd string) string {
	matches := setDiceRegex.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return "无效的设置指令格式，正确格式：.set[数字]，例如.set6"
	}

	sides, err := strconv.Atoi(matches[1])
	if err != nil || sides <= 0 {
		return "骰子面数必须是正整数"
	}

	diceMutex.Lock()
	defaultDiceSides = sides
	diceMutex.Unlock()

	return fmt.Sprintf("已设置默认骰子面数为D%d", sides)
}

func processReasonRoll(cmd string) string {
	matches := reasonRollRegex.FindStringSubmatch(cmd)
	if len(matches) < 3 {
		return "无效的投掷指令格式"
	}

	reason := strings.TrimSpace(matches[2])
	if reason == "" {
		return "请输入投掷理由，例如：.r 测试"
	}

	diceMutex.RLock()
	sides := defaultDiceSides
	diceMutex.RUnlock()

	roll := rand.Intn(sides) + 1
	return fmt.Sprintf("因为 %s 1D%d=%d", reason, sides, roll)
}

func processRACheck(cmd string) string {
	matches := raRegex.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return "无效的ra指令格式，正确格式：.ra 技能值"
	}

	skillValue, err := strconv.Atoi(matches[1])
	if err != nil || skillValue < 1 || skillValue > 100 {
		return "技能值必须为1-100的整数"
	}

	roll := rand.Intn(100) + 1
	result := fmt.Sprintf("检定ra %d → %d", skillValue, roll)

	if roll <= skillValue {
		if roll <= 5 {
			result += " 大成功！"
		} else {
			result += " 成功"
		}
	} else {
		if roll >= 96 {
			result += " 大失败！"
		} else {
			result += " 失败"
		}
	}
	return result
}

func processRBCheck(cmd string) string {
	matches := rbRegex.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return "无效的rb指令格式，正确格式：.rb 技能值"
	}

	skillValue, err := strconv.Atoi(matches[1])
	if err != nil || skillValue < 1 || skillValue > 100 {
		return "技能值必须为1-100的整数"
	}

	bonusDice := rand.Intn(10) * 10
	roll := rand.Intn(100) + 1
	effectiveRoll := roll
	if bonusDice < roll {
		effectiveRoll = bonusDice
	}

	result := fmt.Sprintf("奖励骰检定rb %d → 原始值:%d 奖励骰:%d 最终值:%d", skillValue, roll, bonusDice, effectiveRoll)

	if effectiveRoll <= skillValue {
		if effectiveRoll <= 5 {
			result += " 大成功！"
		} else {
			result += " 成功"
		}
	} else {
		if effectiveRoll >= 96 {
			result += " 大失败！"
		} else {
			result += " 失败"
		}
	}
	return result
}

func processRCCheck(cmd string) string {
	matches := rcRegex.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return "无效的rc指令格式，正确格式：.rc 技能值"
	}

	skillValue, err := strconv.Atoi(matches[1])
	if err != nil || skillValue < 1 || skillValue > 100 {
		return "技能值必须为1-100的整数"
	}

	roll := rand.Intn(100) + 1
	result := fmt.Sprintf("检定rc %d → %d", skillValue, roll)

	success := roll <= skillValue
	var critical bool
	if skillValue <= 50 {
		critical = roll <= 5
	} else {
		critical = roll <= skillValue/5
	}
	critical = critical && roll <= 95
	extreme := success && (roll <= skillValue/5)
	hard := success && (roll <= skillValue/2)
	fumble := roll == 100 || (roll >= 96 && !success)

	switch {
	case success && critical:
		result += " 大成功！"
	case success && extreme:
		result += " 极难成功"
	case success && hard:
		result += " 困难成功"
	case success:
		result += " 成功"
	case fumble:
		result += " 大失败！"
	default:
		result += " 失败"
	}
	return result
}

func processEnCheck(cmd string) string {
	matches := enRegex.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return "无效的en指令格式，正确格式：.en 技能值"
	}

	skillValue, err := strconv.Atoi(matches[1])
	if err != nil || skillValue < 1 || skillValue > 100 {
		return "技能值必须为1-100的整数"
	}

	roll := rand.Intn(100) + 1
	if roll > skillValue {
		increase := rand.Intn(10) + 1
		newSkill := skillValue + increase
		if newSkill > 100 {
			newSkill = 100
		}
		return fmt.Sprintf("技能成长检定en %d → %d/%d 成功！技能提升%d点，新值:%d", skillValue, roll, skillValue, increase, newSkill)
	}
	return fmt.Sprintf("技能成长检定en %d → %d/%d 失败", skillValue, roll, skillValue)
}

func processTICheck() string {
	symptoms := []string{
		"1. 失忆: 调查员会发现自己只记得最后身处的安全地点，却没有任何来到这里的记忆。",
		"2. 假性残疾: 调查员陷入了心理性的失明，失聪以及躯体缺失感中。",
		"3. 暴力倾向: 调查员陷入了六亲不认的暴力行为中。",
		"4. 偏执: 调查员陷入了严重的偏执妄想之中。",
		"5. 人际依赖: 调查员因为一些原因而将他人误认为了他重要的人。",
		"6. 昏厥: 调查员当场昏倒。",
		"7. 逃避行为: 调查员会用任何的手段试图逃离现在所处的位置。",
		"8. 竭嘶底里: 调查员表现出大笑，哭泣，嘶吼，害怕等的极端情绪表现。",
		"9. 恐惧: 调查员投一个D100或者由守秘人选择，来从恐惧症状表中选择一个恐惧源。",
		"10. 狂躁: 调查员投一个D100或者由守秘人选择，来从狂躁症状表中选择一个狂躁的表现。",
	}
	roll := rand.Intn(10)
	return "临时疯狂症状:\n" + symptoms[roll]
}

func processLICheck() string {
	symptoms := []string{
		"1. 失忆: 回过神来，调查员们发现自己身处一个陌生的地方，并忘记了自己是谁。",
		"2. 被窃: 调查员在1D10小时后恢复清醒，发觉自己被盗，身体毫发无损。",
		"3. 遍体鳞伤: 调查员在1D10小时后恢复清醒，发现自己身上满是拳痕和瘀伤。",
		"4. 暴力倾向: 调查员陷入强烈的暴力与破坏欲之中。",
		"5. 极端信念: 调查员在1D10小时后恢复清醒，遵循着某个启示。",
		"6. 重要之人: 调查员在1D10小时后恢复清醒，决定与某人或某物建立深厚的联系。",
		"7. 被收容: 调查员在1D10小时后恢复清醒，发现自己在监狱或精神病院。",
		"8. 逃避行为: 调查员恢复清醒时发现自己在陌生的地方，可能无意识旅行了很远。",
		"9. 恐惧: 投一个D100或者由守秘人选择，来从恐惧症状表中选择一个恐惧源。",
		"10. 狂躁: 投一个D100或者由守秘人选择，来从狂躁症状表中选择一个狂躁的表现。",
	}
	roll := rand.Intn(10)
	return "总结性疯狂症状:\n" + symptoms[roll]
}

func processStCheck(cmd string) string {
	matches := stRegex.FindStringSubmatch(cmd)
	if len(matches) < 3 {
		return "无效的st指令格式，正确格式：.st [技能名] [数值] (可多个)"
	}

	var result strings.Builder
	result.WriteString("记录属性:\n")

	for i := 1; i < len(matches); i += 2 {
		if i+1 >= len(matches) {
			break
		}

		skillName := strings.TrimSpace(matches[i])
		skillValue := strings.TrimSpace(matches[i+1])

		if skillName == "" || skillValue == "" {
			continue
		}

		value, err := strconv.Atoi(skillValue)
		if err != nil {
			result.WriteString(fmt.Sprintf("无效数值: %s = %s\n", skillName, skillValue))
			continue
		}

		result.WriteString(fmt.Sprintf("%s = %d\n", skillName, value))
	}

	return result.String()
}

func processRoll(cmd string) string {
	cmd = strings.TrimPrefix(cmd, "r")
	cmd = strings.TrimSpace(cmd)

	if strings.Contains(cmd, "d") {
		parts := strings.FieldsFunc(cmd, func(r rune) bool {
			return r == '+' || r == '-' || r == '*'
		})
		ops := make([]string, 0)
		for _, r := range cmd {
			if r == '+' || r == '-' || r == '*' {
				ops = append(ops, string(r))
			}
		}

		var results []string
		total := 0
		lastOp := "+"

		for i, part := range parts {
			if i > 0 && i-1 < len(ops) {
				lastOp = ops[i-1]
			}

			if strings.Contains(part, "d") {
				diceParts := strings.Split(part, "d")
				diceNum := 1
				diceSides := 0
				var err error

				if diceParts[0] != "" {
					diceNum, err = strconv.Atoi(diceParts[0])
					if err != nil {
						return "无效的骰子数量"
					}
				}

				if len(diceParts) < 2 {
					return "无效的骰子表达式"
				}

				diceSides, err = strconv.Atoi(diceParts[1])
				if err != nil {
					return "无效的骰子面数"
				}

				if diceNum <= 0 || diceSides <= 0 {
					return "骰子数量和面数必须为正整数"
				}

				var rolls []int
				sum := 0
				for j := 0; j < diceNum; j++ {
					roll := rand.Intn(diceSides) + 1
					rolls = append(rolls, roll)
					sum += roll
				}

				switch lastOp {
				case "+":
					total += sum
				case "-":
					total -= sum
				case "*":
					total *= sum
				}

				results = append(results, fmt.Sprintf("%dd%d: %v = %d", diceNum, diceSides, rolls, sum))
			} else {
				num, err := strconv.Atoi(part)
				if err != nil {
					return "无效的数字"
				}

				switch lastOp {
				case "+":
					total += num
				case "-":
					total -= num
				case "*":
					total *= num
				}

				results = append(results, fmt.Sprintf("%d", num))
			}
		}

		var builder strings.Builder
		builder.WriteString("掷骰: ")
		for i, res := range results {
			if i > 0 {
				builder.WriteString(fmt.Sprintf(" %s ", ops[i-1]))
			}
			builder.WriteString(res)
		}
		builder.WriteString(fmt.Sprintf(" = %d", total))
		return builder.String()
	}

	diceMutex.RLock()
	sides := defaultDiceSides
	diceMutex.RUnlock()

	num, err := strconv.Atoi(cmd)
	if err != nil {
		return "无效的骰子指令格式"
	}

	roll := rand.Intn(sides) + 1
	return fmt.Sprintf("掷骰 1D%d: %d", sides, roll)
}

func processCoC7() string {
	var attributes []string
	for _, attr := range cocAttributes {
		var value int
		switch attr {
		case "STR", "CON":
			value = rand.Intn(6)*5 + 30
		case "SIZ":
			value = rand.Intn(6)+rand.Intn(6)+6
		case "DEX":
			value = rand.Intn(6)*5 + 30
		case "APP":
			value = rand.Intn(6)*5 + 30
		case "INT":
			value = rand.Intn(6)*5 + 50
		case "POW":
			value = rand.Intn(6)*5 + 30
		case "EDU":
			value = rand.Intn(6)*5 + 30
		case "LUK":
			value = rand.Intn(6)*5 + 30
		}
		attributes = append(attributes, fmt.Sprintf("%s: %d", attr, value))
	}
	return "调查员属性(7版规则):\n" + strings.Join(attributes, "\n")
}

func processSanCheck(cmd string) string {
	matches := scRegex.FindStringSubmatch(cmd)
	if len(matches) < 3 {
		return "理智检定格式错误，正确格式：.sc 成功损失/失败损失"
	}
	successLoss, _ := strconv.Atoi(matches[1])
	failLoss, _ := strconv.Atoi(matches[2])

	roll := rand.Intn(100) + 1
	result := fmt.Sprintf("理智检定: %d/%d 掷骰: %d", successLoss, failLoss, roll)
	return result
}

func broadcastToWeb(message string) {
	webMutex.RLock()
	defer webMutex.RUnlock()

	for client := range webClients {
		client := client
		go func() {
			if err := client.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				log.Printf("广播消息失败: %v", err)
				webMutex.Lock()
				delete(webClients, client)
				client.Close()
				webMutex.Unlock()
			}
		}()
	}
}

func handleWebCommand(command string) {
	response := processCommand(command)
	broadcastToWeb(response)

	if err := sendToQQ(response); err != nil {
		log.Printf("发送到QQ失败: %v", err)
	}
}

func sendToQQ(message string) error {
	conn, err := qqManager.Get()
	if err != nil {
		if err := qqManager.Connect(); err != nil {
			return fmt.Errorf("连接失败: %w", err)
		}
		conn, _ = qqManager.Get()
	}

	resp := ResponseMessage{
		Action: "send_msg",
		Params: map[string]interface{}{
			"message_type": "group",
			"group_id":     appConfig.QQGroupID,
			"message":      message,
		},
	}

	if err := conn.WriteJSON(resp); err != nil {
		qqManager.mu.Lock()
		qqManager.conn = nil
		qqManager.mu.Unlock()
		return fmt.Errorf("写入失败: %w", err)
	}
	return nil
}

func sendResponse(conn *websocket.Conn, msg *OneBotMessage, response string) error {
	params := map[string]interface{}{
		"message_type": msg.MessageType,
		"message":      response,
	}

	if msg.MessageType == "group" {
		params["group_id"] = msg.GroupID
	} else {
		params["user_id"] = msg.UserID
	}

	resp := ResponseMessage{
		Action: "send_msg",
		Params: params,
	}

	if err := conn.WriteJSON(resp); err != nil {
		return fmt.Errorf("发送响应失败: %w", err)
	}
	return nil
}
