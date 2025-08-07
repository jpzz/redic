package redic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gomodule/redigo/redis"
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
		InitialDelay:      time.Second,
		MaxDelay:          30 * time.Second,
		BackoffMultiplier: 2.0,
		Jitter:            true,
		PingTimeout:       5 * time.Second,
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
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
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

	maxWait := 10 * time.Second
	checkInterval := 200 * time.Millisecond

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

	// 状态管理
	state int32 // atomic ConnectionState

	// 重连控制
	reconnectMu     sync.Mutex
	reconnectCtx    context.Context
	reconnectCancel context.CancelFunc
	reconnecting    int32 // atomic
	currentAttempt  int32 // atomic

	// 订阅管理
	subManager *SubscriptionManager

	// 控制
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// 创建客户端
func NewClient(addr string) *Client {
	return NewClientWithConfig(addr, "", 0, DefaultReconnectConfig())
}

func NewClientWithConfig(addr, password string, database int, config *ReconnectConfig) *Client {
	if config == nil {
		config = DefaultReconnectConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		addr:            addr,
		password:        password,
		database:        database,
		reconnectConfig: config,
		ctx:             ctx,
		cancel:          cancel,
	}

	client.setState(StateDisconnected)
	client.initPool()
	client.subManager = NewSubscriptionManager(client.pool, client)

	return client
}

// 状态管理
func (c *Client) setState(state ConnectionState) {
	atomic.StoreInt32(&c.state, int32(state))
}

func (c *Client) GetState() ConnectionState {
	return ConnectionState(atomic.LoadInt32(&c.state))
}

// 初始化连接池
func (c *Client) initPool() {
	c.pool = &redis.Pool{
		MaxIdle:     10,
		MaxActive:   100,
		IdleTimeout: 300 * time.Second,
		Wait:        true,

		Dial: func() (redis.Conn, error) {
			return c.createConnection()
		},

		TestOnBorrow: func(conn redis.Conn, t time.Time) error {
			if time.Since(t) < 10*time.Minute {
				return nil
			}
			return c.pingConnection(conn)
		},
	}
}

// 创建连接
func (c *Client) createConnection() (redis.Conn, error) {
	c.setState(StateConnecting)
	if c.reconnectConfig.OnConnecting != nil {
		go c.reconnectConfig.OnConnecting()
	}

	dialOptions := []redis.DialOption{
		redis.DialConnectTimeout(5 * time.Second),
		redis.DialReadTimeout(30 * time.Second),
		redis.DialWriteTimeout(10 * time.Second),
		redis.DialKeepAlive(60 * time.Second),
	}

	if c.password != "" {
		dialOptions = append(dialOptions, redis.DialPassword(c.password))
	}

	if c.database != 0 {
		dialOptions = append(dialOptions, redis.DialDatabase(c.database))
	}

	conn, err := redis.Dial("tcp", c.addr, dialOptions...)

	if err != nil {
		c.handleConnectionError(err)
		return nil, err
	}

	c.handleConnectionSuccess()
	return conn, nil
}

// 处理连接成功
func (c *Client) handleConnectionSuccess() {
	c.setState(StateConnected)
	atomic.StoreInt32(&c.currentAttempt, 0)
	atomic.StoreInt32(&c.reconnecting, 0)

	if c.reconnectConfig.OnConnected != nil {
		go c.reconnectConfig.OnConnected()
	}

	log.Printf("Redis连接成功: %s", c.addr)

	go func() {
		time.Sleep(time.Second)
		if err := c.subManager.ResubscribeAll(); err != nil {
			log.Printf("重新订阅失败: %v", err)
		}
	}()
}

// 处理连接错误
func (c *Client) handleConnectionError(err error) {
	oldState := c.GetState()
	if oldState == StateReconnecting || oldState == StateConnecting {
		return
	}
	c.setState(StateDisconnected)
	if c.reconnectConfig.OnDisconnected != nil {
		go c.reconnectConfig.OnDisconnected(err)
	}

	log.Printf("Redis连接失败: %v", err)
	c.triggerReconnect()
}

// Ping连接测试
func (c *Client) pingConnection(conn redis.Conn) error {
	if c.reconnectConfig.PingTimeout > 0 {
		if tcpConn, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			tcpConn.SetDeadline(time.Now().Add(c.reconnectConfig.PingTimeout))
			defer tcpConn.SetDeadline(time.Time{})
		}
	}

	_, err := conn.Do("PING")
	if err != nil && isNetworkError(err) && c.GetState() != StateReconnecting {
		c.handleConnectionError(err)
	}

	return err
}

// 触发重连
func (c *Client) triggerReconnect() {
	if !atomic.CompareAndSwapInt32(&c.reconnecting, 0, 1) {
		return
	}

	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	if c.reconnectCancel != nil {
		c.reconnectCancel()
	}

	c.reconnectCtx, c.reconnectCancel = context.WithCancel(c.ctx)
	c.wg.Add(1)
	go c.reconnectLoop()
}

// 重连循环
func (c *Client) reconnectLoop() {
	defer c.wg.Done()
	defer atomic.StoreInt32(&c.reconnecting, 0)

	c.setState(StateReconnecting)
	attempt := int(atomic.AddInt32(&c.currentAttempt, 1))

	if c.reconnectConfig.OnReconnecting != nil {
		go c.reconnectConfig.OnReconnecting(attempt)
	}

	log.Printf("开始重连尝试 #%d", attempt)

	for {
		select {
		case <-c.reconnectCtx.Done():
			return
		default:
		}

		if c.reconnectConfig.MaxRetries > 0 && attempt > c.reconnectConfig.MaxRetries {
			c.setState(StateFailed)
			if c.reconnectConfig.OnGiveUp != nil {
				go c.reconnectConfig.OnGiveUp(fmt.Errorf("超过最大重试次数: %d", c.reconnectConfig.MaxRetries))
			}
			log.Printf("重连失败，超过最大重试次数: %d", c.reconnectConfig.MaxRetries)
			return
		}

		delay := c.calculateDelay(attempt)
		log.Printf("重连尝试 #%d，%v后重试", attempt, delay)

		select {
		case <-c.reconnectCtx.Done():
			return
		case <-time.After(delay):
		}

		if c.attemptReconnect() {
			if c.reconnectConfig.OnReconnected != nil {
				go c.reconnectConfig.OnReconnected(attempt)
			}

			log.Printf("重连成功，尝试次数: %d", attempt)
			return
		}

		attempt = int(atomic.AddInt32(&c.currentAttempt, 1))
	}
}

// 计算重连延迟
func (c *Client) calculateDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return c.reconnectConfig.InitialDelay
	}

	base := float64(c.reconnectConfig.InitialDelay)
	max := float64(c.reconnectConfig.MaxDelay)

	delay := base * math.Pow(c.reconnectConfig.BackoffMultiplier, float64(attempt-1))

	if delay > max {
		delay = max
	}

	if c.reconnectConfig.Jitter {
		jitterRange := delay * 0.15
		jitter := (2*jitterRange)*float64(time.Now().UnixNano()%1000)/1000 - jitterRange
		delay += jitter
	}

	return time.Duration(delay)
}

// 尝试重连
func (c *Client) attemptReconnect() bool {
	conn := c.pool.Get()
	defer conn.Close()

	if tcpConn, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		tcpConn.SetDeadline(time.Now().Add(5 * time.Second))
		defer tcpConn.SetDeadline(time.Time{})
	}

	_, err := conn.Do("PING")
	if err != nil {
		log.Printf("重连测试失败: %v", err)
		return false
	}

	c.setState(StateConnected)
	atomic.StoreInt32(&c.currentAttempt, 0)
	atomic.StoreInt32(&c.reconnecting, 0)

	if c.reconnectConfig.OnConnected != nil {
		go c.reconnectConfig.OnConnected()
	}

	log.Printf("Redis重连成功: %s", c.addr)

	go func() {
		time.Sleep(time.Second)
		if err := c.subManager.ResubscribeAll(); err != nil {
			log.Printf("重新订阅失败: %v", err)
		}
	}()

	return true
}

// 基础Redis操作
func (c *Client) Do(commandName string, args ...interface{}) (interface{}, error) {
	conn := c.pool.Get()
	defer conn.Close()

	reply, err := conn.Do(commandName, args...)
	if err != nil && isNetworkError(err) {
		c.handleConnectionError(err)
	}

	return reply, err
}

func (c *Client) Get(key string) (string, error) {
	result, err := c.Do("GET", key)
	if err != nil {
		return "", err
	}

	if result == nil {
		return "", redis.ErrNil
	}

	return string(result.([]byte)), nil
}

func (c *Client) Set(key string, value interface{}) error {
	_, err := c.Do("SET", key, value)
	return err
}

func (c *Client) Del(keys ...string) (int64, error) {
	args := make([]interface{}, len(keys))
	for i, key := range keys {
		args[i] = key
	}

	result, err := c.Do("DEL", args...)
	if err != nil {
		return 0, err
	}

	return result.(int64), nil
}

func (c *Client) Ping() error {
	_, err := c.Do("PING")
	return err
}

// 订阅功能
func (c *Client) Subscribe(channel string, handler func(channel, message string)) error {
	return c.subManager.Subscribe(channel, handler)
}

func (c *Client) PSubscribe(pattern string, handler func(channel, message string)) error {
	return c.subManager.PSubscribe(pattern, handler)
}

func (c *Client) Unsubscribe(channels ...string) error {
	return c.subManager.Unsubscribe(channels...)
}

func (c *Client) PUnsubscribe(patterns ...string) error {
	return c.subManager.PUnsubscribe(patterns...)
}

func (c *Client) Publish(channel string, message interface{}) (int64, error) {
	result, err := c.Do("PUBLISH", channel, message)
	if err != nil {
		return 0, err
	}

	return result.(int64), nil
}

// 获取连接信息
func (c *Client) GetConnectionInfo() map[string]interface{} {
	return map[string]interface{}{
		"state":           c.GetState().String(),
		"current_attempt": atomic.LoadInt32(&c.currentAttempt),
		"subscriptions":   c.subManager.GetSubscriptionCount(),
		"reconnecting":    atomic.LoadInt32(&c.reconnecting) == 1,
	}
}

// 关闭客户端
func (c *Client) Close() error {
	log.Println("开始关闭Redis客户端...")

	c.cancel()

	if c.reconnectCancel != nil {
		c.reconnectCancel()
	}

	if c.subManager != nil {
		c.subManager.Close()
	}

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("所有协程已正常结束")
	case <-time.After(10 * time.Second):
		log.Println("等待协程结束超时，强制关闭")
	}

	var err error
	if c.pool != nil {
		err = c.pool.Close()
	}

	log.Println("Redis客户端已关闭")
	return err
}

// 网络错误检查
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if isNonNetworkError(err) {
		return false
	}
	if isKnownNetworkError(err) {
		return true
	}
	if isNetworkInterfaceError(err) {
		return true
	}
	if isNetworkSyscallError(err) {
		return true
	}

	return false
}

// 检查已知的非网络错误
func isNonNetworkError(err error) bool {
	if errors.Is(err, redis.ErrNil) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

// 检查已知的网络错误
func isKnownNetworkError(err error) bool {
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	if errors.Is(err, redis.ErrPoolExhausted) {
		return true
	}

	return false
}

func isNetworkInterfaceError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		return isNetworkSyscallErrno(syscallErr)
	}

	return false
}

func isNetworkSyscallError(err error) bool {
	networkErrnos := []error{
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.EPIPE,
		syscall.ETIMEDOUT,
		syscall.ENETDOWN,
		syscall.ENETUNREACH,
		syscall.ENETRESET,
		syscall.ENOTCONN,
		syscall.ESHUTDOWN,
		syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH,
	}

	for _, errno := range networkErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}

	return false
}

func isNetworkSyscallErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.EPIPE,
		syscall.ETIMEDOUT,
		syscall.ENETDOWN,
		syscall.ENETUNREACH,
		syscall.ENETRESET,
		syscall.ENOTCONN,
		syscall.ESHUTDOWN,
		syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH:
		return true
	default:
		return false
	}
}
