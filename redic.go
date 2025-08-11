package redic

import (
	"context"
	"errors"
	"io"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gomodule/redigo/redis"
)

var nonNetworkErrors = []error{
	redis.ErrNil,
	context.Canceled,
	context.DeadlineExceeded,
}

var knownNetworkErrors = []error{
	io.EOF,
	io.ErrUnexpectedEOF,
	io.ErrClosedPipe,
	os.ErrDeadlineExceeded,
	redis.ErrPoolExhausted,
}

var networkSyscallErrnos = map[syscall.Errno]struct{}{
	syscall.ECONNRESET:   {},
	syscall.ECONNABORTED: {},
	syscall.ECONNREFUSED: {},
	syscall.EPIPE:        {},
	syscall.ETIMEDOUT:    {},
	syscall.ENETDOWN:     {},
	syscall.ENETUNREACH:  {},
	syscall.ENETRESET:    {},
	syscall.ENOTCONN:     {},
	syscall.ESHUTDOWN:    {},
	syscall.EHOSTDOWN:    {},
	syscall.EHOSTUNREACH: {},
}

const (
	defaultInitialDelay             = 1 * time.Second
	defaultMaxDelay                 = 30 * time.Second
	defaultBackoffMultiplier        = 2.0
	defaultPingTimeout              = 5 * time.Second
	defaultReadDeadline             = 60 * time.Second
	defaultResubscribeMaxWait       = 10 * time.Second
	defaultResubscribeCheckInterval = 200 * time.Millisecond
)

// 连接状态
type ConnectionState int32

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateFailed
)

func (s ConnectionState) String() string {
	states := []string{"Disconnected", "Connecting", "Connected", "Reconnecting", "Failed"}
	if int(s) < len(states) {
		return states[s]
	}
	return "Unknown"
}

// 重连配置
type ReconnectConfig struct {
	MaxRetries        int           // 最大重试次数，-1表示无限重试
	InitialDelay      time.Duration // 初始重连延迟
	MaxDelay          time.Duration // 最大重连延迟
	BackoffMultiplier float64       // 退避倍数
	Jitter            bool          // 是否添加随机抖动
	PingTimeout       time.Duration // Ping超时时间

	// 事件回调
	OnConnecting   func()
	OnConnected    func()
	OnDisconnected func(error)
	OnReconnecting func(attempt int)
	OnReconnected  func(attempt int)
	OnGiveUp       func(error)
}

// 默认配置
func DefaultReconnectConfig() *ReconnectConfig {
	return &ReconnectConfig{
		MaxRetries:        -1, // 无限重试
		InitialDelay:      defaultInitialDelay,
		MaxDelay:          defaultMaxDelay,
		BackoffMultiplier: defaultBackoffMultiplier,
		Jitter:            true,
		PingTimeout:       defaultPingTimeout,
	}
}

// 订阅信息
type SubscriptionInfo struct {
	Channel   string
	Pattern   string
	Handler   func(channel, message string)
	IsPattern bool
	Active    int32 // atomic
}

func (s *SubscriptionInfo) Key() string {
	if s.IsPattern {
		return "pattern:" + s.Pattern
	}
	return "channel:" + s.Channel
}

func (s *SubscriptionInfo) IsActive() bool {
	return atomic.LoadInt32(&s.Active) == 1
}

func (s *SubscriptionInfo) SetActive(active bool) {
	if active {
		atomic.StoreInt32(&s.Active, 1)
	} else {
		atomic.StoreInt32(&s.Active, 0)
	}
}

// 订阅管理器
type SubscriptionManager struct {
	mu            sync.RWMutex
	subscriptions map[string]*SubscriptionInfo
	pubsubConn    *redis.PubSubConn
	connMu        sync.Mutex
	pool          *redis.Pool
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	client        *Client

	receiving     int32
	resubscribing int32
}

func NewSubscriptionManager(pool *redis.Pool, client *Client) *SubscriptionManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &SubscriptionManager{
		subscriptions: make(map[string]*SubscriptionInfo),
		pool:          pool,
		ctx:           ctx,
		cancel:        cancel,
		client:        client,
	}
}

func (sm *SubscriptionManager) Subscribe(channel string, handler func(channel, message string)) error {
	sub := &SubscriptionInfo{
		Channel:   channel,
		Handler:   handler,
		IsPattern: false,
	}
	sub.SetActive(true)

	sm.mu.Lock()
	sm.subscriptions[sub.Key()] = sub
	sm.mu.Unlock()

	return sm.doSubscribe(channel, false)
}

func (sm *SubscriptionManager) PSubscribe(pattern string, handler func(channel, message string)) error {
	sub := &SubscriptionInfo{
		Pattern:   pattern,
		Handler:   handler,
		IsPattern: true,
	}
	sub.SetActive(true)

	sm.mu.Lock()
	sm.subscriptions[sub.Key()] = sub
	sm.mu.Unlock()

	return sm.doSubscribe(pattern, true)
}

func (sm *SubscriptionManager) doSubscribe(channelOrPattern string, isPattern bool) error {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	if sm.pubsubConn == nil {
		conn := sm.pool.Get()
		sm.pubsubConn = &redis.PubSubConn{Conn: conn}
		if atomic.CompareAndSwapInt32(&sm.receiving, 0, 1) {
			sm.wg.Add(1)
			go sm.subscriptionReceiver()
		}
	}

	var err error
	if isPattern {
		err = sm.pubsubConn.PSubscribe(channelOrPattern)
	} else {
		err = sm.pubsubConn.Subscribe(channelOrPattern)
	}

	if err != nil && isNetworkError(err) {
		sm.handleConnectionError(err)
		return err
	}

	log.Printf("订阅成功: %s (pattern: %v)", channelOrPattern, isPattern)
	return err
}

func (sm *SubscriptionManager) Unsubscribe(channels ...string) error {
	for _, channel := range channels {
		key := "channel:" + channel
		sm.mu.Lock()
		if sub, exists := sm.subscriptions[key]; exists {
			sub.SetActive(false)
			delete(sm.subscriptions, key)
		}
		sm.mu.Unlock()
	}

	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	if sm.pubsubConn != nil {
		args := make([]interface{}, len(channels))
		for i, channel := range channels {
			args[i] = channel
		}
		return sm.pubsubConn.Unsubscribe(args...)
	}

	return nil
}

func (sm *SubscriptionManager) PUnsubscribe(patterns ...string) error {
	for _, pattern := range patterns {
		key := "pattern:" + pattern
		sm.mu.Lock()
		if sub, exists := sm.subscriptions[key]; exists {
			sub.SetActive(false)
			delete(sm.subscriptions, key)
		}
		sm.mu.Unlock()
	}

	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	if sm.pubsubConn != nil {
		args := make([]interface{}, len(patterns))
		for i, pattern := range patterns {
			args[i] = pattern
		}
		return sm.pubsubConn.PUnsubscribe(args...)
	}

	return nil
}

// 消息接收器
func (sm *SubscriptionManager) subscriptionReceiver() {
	defer sm.wg.Done()
	defer atomic.StoreInt32(&sm.receiving, 0)

	for {
		select {
		case <-sm.ctx.Done():
			return
		default:
		}

		sm.connMu.Lock()
		if sm.pubsubConn == nil {
			sm.connMu.Unlock()
			time.Sleep(time.Second)
			continue
		}

		if conn, ok := sm.pubsubConn.Conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			conn.SetReadDeadline(time.Now().Add(defaultReadDeadline))
		}

		msg := sm.pubsubConn.Receive()
		sm.connMu.Unlock()

		if msg == nil {
			continue
		}

		sm.handleMessage(msg)
	}
}

func (sm *SubscriptionManager) handleMessage(msg interface{}) {
	switch v := msg.(type) {
	case redis.Message:
		key := "channel:" + v.Channel
		sm.mu.RLock()
		sub, ok := sm.subscriptions[key]
		sm.mu.RUnlock()

		if ok && sub.IsActive() && sub.Handler != nil {
			go sm.safeHandleMessage(sub.Handler, v.Channel, string(v.Data))
		}

	case redis.Subscription:
		log.Printf("订阅状态: %s %s (count: %d)", v.Kind, v.Channel, v.Count)

	case redis.Pong:
		log.Println("收到 Pong 消息")

	case error:
		if isNetworkError(v) {
			log.Printf("订阅连接错误: %v", v)
			sm.handleConnectionError(v)
		} else {
			log.Printf("订阅其他错误: %v", v)
		}

	default:
		if sm.tryHandlePatternMessage(msg) {
			return
		}
		log.Printf("收到未知类型消息: %T %+v", msg, msg)
	}
}

// 尝试处理模式消息
func (sm *SubscriptionManager) tryHandlePatternMessage(msg interface{}) bool {
	// 处理 Redis 的原生模式消息结构
	if arr, ok := msg.([]interface{}); ok && len(arr) >= 4 {
		if msgType, ok := arr[0].([]byte); ok && string(msgType) == "pmessage" {
			if pattern, ok := arr[1].([]byte); ok {
				if channel, ok := arr[2].([]byte); ok {
					if data, ok := arr[3].([]byte); ok {
						sm.handlePatternMessage(string(pattern), string(channel), string(data))
						return true
					}
				}
			}
		}
	}

	if msgMap, ok := msg.(map[string]interface{}); ok {
		if msgType, exists := msgMap["type"]; exists && msgType == "pmessage" {
			pattern, _ := msgMap["pattern"].(string)
			channel, _ := msgMap["channel"].(string)
			data, _ := msgMap["data"].(string)
			if pattern != "" && channel != "" {
				sm.handlePatternMessage(pattern, channel, data)
				return true
			}
		}
	}

	return false
}

// 处理模式消息
func (sm *SubscriptionManager) handlePatternMessage(pattern, channel, message string) {
	key := "pattern:" + pattern
	sm.mu.RLock()
	sub, ok := sm.subscriptions[key]
	sm.mu.RUnlock()

	if ok && sub.IsActive() && sub.Handler != nil {
		go sm.safeHandleMessage(sub.Handler, channel, message)
	}
}

func (sm *SubscriptionManager) safeHandleMessage(handler func(string, string), channel, message string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("消息处理异常 [%s]: %v", channel, r)
		}
	}()
	handler(channel, message)
}

func (sm *SubscriptionManager) handleConnectionError(err error) {
	sm.connMu.Lock()
	if sm.pubsubConn != nil {
		sm.pubsubConn.Close()
		sm.pubsubConn = nil
	}
	sm.connMu.Unlock()

	if sm.client != nil {
		sm.client.handleConnectionError(err)
	}
}

// 重新订阅所有频道
func (sm *SubscriptionManager) ResubscribeAll() error {
	if !atomic.CompareAndSwapInt32(&sm.resubscribing, 0, 1) {
		log.Println("重新订阅已在进行中，跳过")
		return nil
	}
	defer atomic.StoreInt32(&sm.resubscribing, 0)

	maxWait := defaultResubscribeMaxWait
	checkInterval := defaultResubscribeCheckInterval

	for waited := time.Duration(0); waited < maxWait; waited += checkInterval {
		if sm.client.GetState() == StateConnected {
			time.Sleep(500 * time.Millisecond)
			break
		}
		time.Sleep(checkInterval)
	}

	if sm.client.GetState() != StateConnected {
		return errors.New("连接未稳定，无法重新订阅")
	}

	log.Println("开始重新订阅所有频道...")

	sm.connMu.Lock()
	if sm.pubsubConn != nil {
		sm.pubsubConn.Close()
		sm.pubsubConn = nil
	}
	sm.connMu.Unlock()

	sm.mu.RLock()
	subscriptions := make([]*SubscriptionInfo, 0, len(sm.subscriptions))
	for _, sub := range sm.subscriptions {
		if sub.IsActive() {
			subscriptions = append(subscriptions, sub)
		}
	}
	sm.mu.RUnlock()

	if len(subscriptions) == 0 {
		log.Println("没有需要重新订阅的频道")
		return nil
	}

	var resubCount int
	var failedSubs []string

	for _, sub := range subscriptions {
		var err error
		for retry := 0; retry < 3; retry++ {
			if sub.IsPattern {
				err = sm.doSubscribe(sub.Pattern, true)
			} else {
				err = sm.doSubscribe(sub.Channel, false)
			}

			if err == nil {
				resubCount++
				break
			}

			if retry < 2 {
				time.Sleep(time.Duration(retry+1) * 500 * time.Millisecond) // 减少重试间隔
			}
		}

		if err != nil {
			failedSubs = append(failedSubs, sub.Key())
			log.Printf("重新订阅失败 %s: %v", sub.Key(), err)
		}
	}

	log.Printf("重新订阅完成，成功: %d, 失败: %d", resubCount, len(failedSubs))

	if len(failedSubs) > 0 {
		go sm.retryFailedSubscriptions(failedSubs)
	}

	return nil
}

func (sm *SubscriptionManager) retryFailedSubscriptions(failedSubs []string) {
	time.Sleep(5 * time.Second)

	for _, key := range failedSubs {
		sm.mu.RLock()
		sub, ok := sm.subscriptions[key]
		sm.mu.RUnlock()

		if !ok || !sub.IsActive() {
			continue
		}

		var err error
		if sub.IsPattern {
			err = sm.doSubscribe(sub.Pattern, true)
		} else {
			err = sm.doSubscribe(sub.Channel, false)
		}

		if err == nil {
			log.Printf("延迟重试订阅成功: %s", key)
		} else {
			log.Printf("延迟重试订阅失败: %s - %v", key, err)
		}
	}
}

func (sm *SubscriptionManager) GetSubscriptionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.subscriptions)
}

func (sm *SubscriptionManager) Close() error {
	sm.cancel()

	sm.connMu.Lock()
	if sm.pubsubConn != nil {
		sm.pubsubConn.Close()
		sm.pubsubConn = nil
	}
	sm.connMu.Unlock()

	sm.wg.Wait()
	return nil
}

// Redis客户端
type Client struct {
	// 基础配置
	addr            string
	password        string
	database        int
	reconnectConfig *ReconnectConfig

	// 连接管理
	pool *redis.Pool

	// 订阅管理
	subManager *SubscriptionManager

	// 状态管理
	state int32 // atomic

	// 回调/状态容器
	mu sync.RWMutex
}

// Helper: 判断是否为网络错误
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	for _, ne := range knownNetworkErrors {
		if errors.Is(err, ne) {
			return true
		}
	}
	// 尝试检查 errno
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if _, ok := networkSyscallErrnos[errno]; ok {
			return true
		}
	}
	return false
}

// 颜色辅助：判断是否为非配置信息中需要忽略的错误
func ignoreNonNetworkError(err error) bool {
	for _, ne := range nonNetworkErrors {
		if errors.Is(err, ne) {
			return true
		}
	}
	return false
}

// 构造新的客户端
func NewClient(addr string, password string, db int, cfg *ReconnectConfig) *Client {
	if cfg == nil {
		cfg = DefaultReconnectConfig()
	}
	// 初始化连接池
	pool := &redis.Pool{
		MaxIdle:     10,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			options := []redis.DialOption{
				redis.DialConnectTimeout(5 * time.Second),
				redis.DialReadTimeout(5 * time.Second),
				redis.DialWriteTimeout(5 * time.Second),
			}
			if password != "" {
				options = append(options, redis.DialPassword(password))
			}
			// 指定数据库
			options = append(options, redis.DialDatabase(db))
			return redis.Dial("tcp", addr, options...)
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
	c := &Client{
		addr:            addr,
		password:        password,
		database:        db,
		reconnectConfig: cfg,
		pool:            pool,
		state:           int32(StateDisconnected),
	}
	c.subManager = NewSubscriptionManager(c.pool, c)
	return c
}

// 连接到 Redis，验证连通性并触发就绪状态
func (c *Client) Connect() error {
	atomic.StoreInt32(&c.state, int32(StateConnecting))
	// 回调：正在连接
	if c.reconnectConfig != nil && c.reconnectConfig.OnConnecting != nil {
		c.reconnectConfig.OnConnecting()
	}
	conn := c.pool.Get()
	if conn == nil {
		atomic.StoreInt32(&c.state, int32(StateDisconnected))
		return errors.New("failed to obtain Redis connection")
	}
	defer conn.Close()

	_, err := conn.Do("PING")
	if err != nil {
		atomic.StoreInt32(&c.state, int32(StateDisconnected))
		// 启动重连循环
		c.startReconnectionLoop(err)
		return err
	}

	atomic.StoreInt32(&c.state, int32(StateConnected))
	if c.reconnectConfig != nil && c.reconnectConfig.OnConnected != nil {
		c.reconnectConfig.OnConnected()
	}
	return nil
}

// 内部：启动重连循环
func (c *Client) startReconnectionLoop(firstErr error) {
	cfg := c.reconnectConfig
	if cfg == nil {
		return
	}
	// 防御性：如果已经在连接状态，直接返回
	if atomic.LoadInt32(&c.state) == int32(StateConnected) {
		return
	}

	go func() {
		attempt := 0
		delay := cfg.InitialDelay
		for {
			// 达到最大重试次数时放弃
			if cfg.MaxRetries >= 0 && attempt >= cfg.MaxRetries {
				atomic.StoreInt32(&c.state, int32(StateFailed))
				if cfg.OnGiveUp != nil {
					cfg.OnGiveUp(errors.New("max reconnect attempts reached"))
				}
				return
			}
			// 回调：重新连接
			if cfg.OnReconnecting != nil {
				cfg.OnReconnecting(attempt)
			}
			// 等待
			time.Sleep(delay)

			// 尝试重新连接
			if c.tryReconnect() {
				atomic.StoreInt32(&c.state, int32(StateConnected))
				if cfg.OnReconnected != nil {
					cfg.OnReconnected(attempt)
				}
				return
			}
			// 失败后调整延迟
			attempt++
			// 退避策略与抖动
			if cfg.Jitter {
				j := time.Duration(rand.Int63n(int64(100))) * time.Millisecond
				delay = time.Duration(float64(delay) * cfg.BackoffMultiplier)
				if delay > cfg.MaxDelay {
					delay = cfg.MaxDelay
				}
				delay += j
			} else {
				delay = time.Duration(float64(delay) * cfg.BackoffMultiplier)
				if delay > cfg.MaxDelay {
					delay = cfg.MaxDelay
				}
			}
		}
	}()
}

// 尝试进行一次重新连接检查
func (c *Client) tryReconnect() bool {
	// 使用现有的 pool 进行一次轻量的 PING
	conn := c.pool.Get()
	if conn == nil {
		return false
	}
	defer conn.Close()

	_, err := conn.Do("PING")
	if err != nil {
		return false
	}
	// 成功后，尝试重新订阅/恢复状态
	if c.subManager != nil {
		_ = c.subManager.ResubscribeAll()
	}
	return true
}

// 获取当前状态
func (c *Client) GetState() ConnectionState {
	return ConnectionState(atomic.LoadInt32(&c.state))
}

// 关闭客户端
func (c *Client) Close() error {
	if c.subManager != nil {
		_ = c.subManager.Close()
	}
	if c.pool != nil {
		return c.pool.Close()
	}
	return nil
}

func (c *Client) Subscribe(channel string, handler func(channel, message string)) error {
	if c.subManager == nil {
		c.subManager = NewSubscriptionManager(c.pool, c)
	}
	return c.subManager.Subscribe(channel, handler)
}

func (c *Client) PSubscribe(pattern string, handler func(channel, message string)) error {
	if c.subManager == nil {
		c.subManager = NewSubscriptionManager(c.pool, c)
	}
	return c.subManager.PSubscribe(pattern, handler)
}

func (c *Client) Unsubscribe(channels ...string) error {
	if c.subManager == nil {
		return nil
	}
	return c.subManager.Unsubscribe(channels...)
}

func (c *Client) PUnsubscribe(patterns ...string) error {
	if c.subManager == nil {
		return nil
	}
	return c.subManager.PUnsubscribe(patterns...)
}

func (c *Client) Publish(channel string, message interface{}) error {
	conn := c.pool.Get()
	if conn == nil {
		return errors.New("no available redis connection")
	}
	defer conn.Close()

	_, err := conn.Do("PUBLISH", channel, message)
	return err
}

func (c *Client) Get(key string) (string, error) {
	conn := c.pool.Get()
	if conn == nil {
		return "", errors.New("no available redis connection")
	}
	defer conn.Close()

	return redis.String(conn.Do("GET", key))
}

func (c *Client) Set(key string, value interface{}) error {
	conn := c.pool.Get()
	if conn == nil {
		return errors.New("no available redis connection")
	}
	defer conn.Close()

	_, err := conn.Do("SET", key, value)
	return err
}

func (c *Client) Send(channel string, payload interface{}) error {
	return c.Publish(channel, payload)
}

func (c *Client) handleConnectionError(err error) {
	if c.reconnectConfig != nil && c.reconnectConfig.OnDisconnected != nil {
		c.reconnectConfig.OnDisconnected(err)
	}
	atomic.StoreInt32(&c.state, int32(StateDisconnected))
	c.startReconnectionLoop(err)
}
