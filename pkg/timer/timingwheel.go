package timer

// Package timer 提供高性能时间轮定时器实现。
// 支持 10 万+ 定时器的高效管理，避免大量 time.AfterFunc 造成的 GC 压力。

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// TimingWheel 分层时间轮定时器。
// 采用多级时间轮结构，每级 256 个槽位，支持纳秒精度。
type TimingWheel struct {
	tick      time.Duration // 每格时间间隔
	slotCount int           // 每级槽位数
	slots     [][]*list.List
	current   []int // 每级当前指针
	depth     int   // 时间轮级数

	// 待添加的任务队列
	addCh chan *timerEntry
	// 停止信号
	stopCh chan struct{}
	// 统计
	pending atomic.Int64
	fired   atomic.Int64

	mu      sync.Mutex
	running bool
}

type timerEntry struct {
	expire   time.Time
	period   time.Duration // 0 = 一次性, >0 = 周期性
	callback func()
	round    int // 需要转几圈
	slot     int // 所在槽位
	stopped  atomic.Bool
	ref      *Timer // 关联的 Timer 对象
}

// Timer 定时器句柄。
type Timer struct {
	entry *timerEntry
	wheel *TimingWheel
}

// Stop 停止定时器。
func (t *Timer) Stop() bool {
	if t.entry == nil {
		return false
	}
	return t.entry.stopped.CompareAndSwap(false, true)
}

// Reset 重置定时器。
func (t *Timer) Reset(d time.Duration) bool {
	if t.entry == nil || t.wheel == nil {
		return false
	}
	t.Stop()
	t.entry.stopped.Store(false)
	t.wheel.schedule(t.entry, d)
	return true
}

// NewTimingWheel 创建分层时间轮。
// tick: 最小时间精度（如 10ms）
// slotCount: 每级槽位数（如 256）
// depth: 级数（如 4 级可覆盖 ~194 天）
func NewTimingWheel(tick time.Duration, slotCount, depth int) *TimingWheel {
	if slotCount <= 0 {
		slotCount = 256
	}
	if depth <= 0 {
		depth = 4
	}
	if tick <= 0 {
		tick = 10 * time.Millisecond
	}

	tw := &TimingWheel{
		tick:      tick,
		slotCount: slotCount,
		depth:     depth,
		current:   make([]int, depth),
		addCh:     make(chan *timerEntry, 4096),
		stopCh:    make(chan struct{}),
	}

	// 初始化槽位
	tw.slots = make([][]*list.List, depth)
	for level := 0; level < depth; level++ {
		tw.slots[level] = make([]*list.List, slotCount)
		for i := 0; i < slotCount; i++ {
			tw.slots[level][i] = list.New()
		}
	}

	return tw
}

// Start 启动时间轮。
func (tw *TimingWheel) Start() {
	tw.mu.Lock()
	if tw.running {
		tw.mu.Unlock()
		return
	}
	tw.running = true
	tw.mu.Unlock()

	go tw.tickLoop()
	go tw.addLoop()
}

// Stop 停止时间轮。
func (tw *TimingWheel) Stop() {
	tw.mu.Lock()
	if !tw.running {
		tw.mu.Unlock()
		return
	}
	tw.running = false
	tw.mu.Unlock()

	close(tw.stopCh)
}

// After 在指定延迟后执行回调，返回 Timer 句柄。
func (tw *TimingWheel) After(d time.Duration, cb func()) *Timer {
	entry := &timerEntry{
		expire:   time.Now().Add(d),
		callback: cb,
	}
	t := &Timer{entry: entry, wheel: tw}
	entry.ref = t
	tw.schedule(entry, d)
	return t
}

// AfterFunc 在指定延迟后执行回调（兼容 time.AfterFunc 接口）。
func (tw *TimingWheel) AfterFunc(d time.Duration, cb func()) *Timer {
	return tw.After(d, cb)
}

// SchedulePeriodic 创建周期性定时器。
func (tw *TimingWheel) SchedulePeriodic(period time.Duration, cb func()) *Timer {
	entry := &timerEntry{
		expire:   time.Now().Add(period),
		period:   period,
		callback: cb,
	}
	t := &Timer{entry: entry, wheel: tw}
	entry.ref = t
	tw.schedule(entry, period)
	return t
}

// Pending 返回待执行的定时器数量。
func (tw *TimingWheel) Pending() int64 {
	return tw.pending.Load()
}

// Fired 返回已触发的定时器总数。
func (tw *TimingWheel) Fired() int64 {
	return tw.fired.Load()
}

func (tw *TimingWheel) schedule(entry *timerEntry, d time.Duration) {
	tw.pending.Add(1)
	select {
	case tw.addCh <- entry:
	default:
		// 队列满，直接处理
		tw.addToWheel(entry)
	}
}

func (tw *TimingWheel) addLoop() {
	for {
		select {
		case entry := <-tw.addCh:
			tw.addToWheel(entry)
		case <-tw.stopCh:
			return
		}
	}
}

func (tw *TimingWheel) addToWheel(entry *timerEntry) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	now := time.Now()
	diff := entry.expire.Sub(now)
	if diff <= 0 {
		// 已过期，解锁后执行
		tw.mu.Unlock()
		tw.fire(entry)
		tw.mu.Lock()
		return
	}

	ticks := int(diff / tw.tick)
	if ticks <= 0 {
		ticks = 1
	}

	// 确定放哪一级
	slot := 0
	round := 0
	remaining := ticks

	for level := 0; level < tw.depth; level++ {
		if remaining < tw.slotCount {
			slot = (tw.current[level] + remaining) % tw.slotCount
			round = 0
			entry.slot = slot
			entry.round = round
			tw.slots[level][slot].PushBack(entry)
			return
		}
		round = remaining / tw.slotCount
		remaining = round

		if level+1 >= tw.depth {
			// 最高级：放在最后一个槽
			slot = tw.slotCount - 1
			tw.slots[level][slot].PushBack(entry)
			entry.slot = slot
			entry.round = remaining
			return
		}
	}
}

func (tw *TimingWheel) tickLoop() {
	ticker := time.NewTicker(tw.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tw.advance()
		case <-tw.stopCh:
			return
		}
	}
}

func (tw *TimingWheel) advance() {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	// 推进第 0 级
	tw.current[0] = (tw.current[0] + 1) % tw.slotCount
	tw.processSlotLocked(0, tw.current[0])

	// 检查是否需要进位
	for level := 0; level < tw.depth-1; level++ {
		if tw.current[level] != 0 {
			break
		}
		tw.current[level+1] = (tw.current[level+1] + 1) % tw.slotCount
		tw.processSlotLocked(level+1, tw.current[level+1])
	}
}

func (tw *TimingWheel) processSlotLocked(level, slot int) {
	l := tw.slots[level][slot]
	if l.Len() == 0 {
		return
	}

	var next *list.Element
	for e := l.Front(); e != nil; e = next {
		next = e.Next()
		entry := e.Value.(*timerEntry)

		if entry.stopped.Load() {
			l.Remove(e)
			tw.pending.Add(-1)
			continue
		}

		if level == 0 {
			// 最低级：触发
			l.Remove(e)
			tw.pending.Add(-1)
			tw.fired.Add(1)
			go entry.callback()
			if entry.period > 0 && !entry.stopped.Load() {
				entry.expire = time.Now().Add(entry.period)
				tw.pending.Add(1)
				tw.slots[0][(tw.current[0]+int(entry.period/tw.tick))%tw.slotCount].PushBack(entry)
			}
		} else {
			// 高级：降级到低一级
			l.Remove(e)
			tw.slots[level-1][slot].PushBack(entry)
		}
	}
}

func (tw *TimingWheel) fire(entry *timerEntry) {
	if entry.stopped.Load() {
		return
	}

	tw.pending.Add(-1)
	tw.fired.Add(1)

	// 执行回调
	go entry.callback()

	// 周期性任务：重新调度
	if entry.period > 0 && !entry.stopped.Load() {
		entry.expire = time.Now().Add(entry.period)
		tw.schedule(entry, entry.period)
	}
}

// ---- 全局默认时间轮 ----

var defaultWheel *TimingWheel

func init() {
	defaultWheel = NewTimingWheel(10*time.Millisecond, 256, 4)
	defaultWheel.Start()
}

// DefaultWheel 返回全局默认时间轮。
func DefaultWheel() *TimingWheel {
	return defaultWheel
}

// After 使用全局时间轮延迟执行。
func After(d time.Duration, cb func()) *Timer {
	return defaultWheel.After(d, cb)
}

// AfterFunc 使用全局时间轮延迟执行。
func AfterFunc(d time.Duration, cb func()) *Timer {
	return defaultWheel.AfterFunc(d, cb)
}
