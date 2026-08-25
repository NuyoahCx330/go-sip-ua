// Package pool 提供高性能内存池和对象池，用于减少 GC 压力和内存分配开销。
// 支持分层内存池（多尺寸类）、零拷贝缓冲区和精确统计。
package pool

import (
	"math"
	"sync"
	"sync/atomic"
)

// ---- 分层内存池 (Tiered Memory Pool) ----

// 预定义缓冲区尺寸类（2 的幂次）。
var sizeClasses = []int{
	64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536,
}

// TieredPool 分层内存池，根据请求大小自动选择最合适的尺寸类。
type TieredPool struct {
	pools   []*sizeClassPool
	maxSize int
	stats   PoolStats
}

type sizeClassPool struct {
	size int
	pool sync.Pool
	gets atomic.Int64
	puts atomic.Int64
}

// PoolStats 内存池统计。
type PoolStats struct {
	TotalGets   atomic.Int64
	TotalPuts   atomic.Int64
	TotalAllocs atomic.Int64
}

// NewTieredPool 创建分层内存池。
func NewTieredPool() *TieredPool {
	tp := &TieredPool{
		maxSize: sizeClasses[len(sizeClasses)-1],
	}
	for _, size := range sizeClasses {
		sc := &sizeClassPool{size: size}
		sc.pool = sync.Pool{
			New: func() interface{} {
				tp.stats.TotalAllocs.Add(1)
				buf := make([]byte, 0, size)
				return &buf
			},
		}
		tp.pools = append(tp.pools, sc)
	}
	return tp
}

// Get 从池中获取指定大小的缓冲区。
func (tp *TieredPool) Get(size int) []byte {
	idx := tp.findSizeClass(size)
	if idx >= len(tp.pools) {
		// 超大请求直接分配
		return make([]byte, 0, size)
	}

	sc := tp.pools[idx]
	sc.gets.Add(1)
	tp.stats.TotalGets.Add(1)

	bufPtr := sc.pool.Get().(*[]byte)
	return (*bufPtr)[:0]
}

// Put 将缓冲区归还到池中。
func (tp *TieredPool) Put(buf []byte) {
	c := cap(buf)
	idx := tp.findSizeClass(c)
	if idx >= len(tp.pools) {
		return // 超大缓冲区直接丢弃给 GC
	}

	sc := tp.pools[idx]
	sc.puts.Add(1)
	tp.stats.TotalPuts.Add(1)

	buf = buf[:0]
	sc.pool.Put(&buf)
}

// findSizeClass 找到能容纳 size 的最小尺寸类索引。
func (tp *TieredPool) findSizeClass(size int) int {
	for i, sc := range tp.pools {
		if sc.size >= size {
			return i
		}
	}
	return len(tp.pools)
}

// Stats 返回池统计。
func (tp *TieredPool) Stats() TieredPoolStats {
	result := TieredPoolStats{
		TotalGets:   tp.stats.TotalGets.Load(),
		TotalPuts:   tp.stats.TotalPuts.Load(),
		TotalAllocs: tp.stats.TotalAllocs.Load(),
	}
	for i, sc := range tp.pools {
		result.SizeClasses = append(result.SizeClasses, SizeClassStat{
			Size: sc.size,
			Gets: sc.gets.Load(),
			Puts: sc.puts.Load(),
		})
		_ = i
	}
	return result
}

// TieredPoolStats 分层池统计。
type TieredPoolStats struct {
	TotalGets   int64
	TotalPuts   int64
	TotalAllocs int64
	SizeClasses []SizeClassStat
}

// SizeClassStat 尺寸类统计。
type SizeClassStat struct {
	Size int
	Gets int64
	Puts int64
}

// ---- ByteBuffer 池 ----

// ByteBuffer 表示一个可复用的字节缓冲区。
type ByteBuffer struct {
	buf  []byte
	off  int
	pool *ByteBufferPool
}

// Bytes 返回缓冲区中未读取的数据。
func (b *ByteBuffer) Bytes() []byte {
	return b.buf[b.off:]
}

// Len 返回未读取的字节数。
func (b *ByteBuffer) Len() int {
	return len(b.buf) - b.off
}

// Cap 返回缓冲区总容量。
func (b *ByteBuffer) Cap() int {
	return cap(b.buf)
}

// Write 向缓冲区追加数据。
func (b *ByteBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf[:b.off+b.Len()], p...)
	return len(p), nil
}

// WriteByte 向缓冲区追加单个字节。
func (b *ByteBuffer) WriteByte(c byte) error {
	b.buf = append(b.buf, c)
	return nil
}

// Read 从缓冲区读取数据。
func (b *ByteBuffer) Read(p []byte) (int, error) {
	n := copy(p, b.buf[b.off:])
	b.off += n
	return n, nil
}

// Reset 重置缓冲区，保留底层数组以供复用。
func (b *ByteBuffer) Reset() {
	b.off = 0
	b.buf = b.buf[:0]
}

// Release 将缓冲区归还到池中。
func (b *ByteBuffer) Release() {
	if b.pool != nil {
		b.Reset()
		b.pool.Put(b)
	}
}

// Grow 确保缓冲区至少有 n 字节可用空间。
func (b *ByteBuffer) Grow(n int) {
	if b.Cap()-b.Len()-b.off >= n {
		return
	}
	newBuf := make([]byte, b.Len(), b.Len()+n)
	copy(newBuf, b.buf[b.off:])
	b.buf = newBuf
	b.off = 0
}

// ByteBufferPool 是 ByteBuffer 的对象池。
type ByteBufferPool struct {
	pool sync.Pool
	size int
	used atomic.Int64
}

// NewByteBufferPool 创建一个新的字节缓冲区池。
func NewByteBufferPool(size int) *ByteBufferPool {
	p := &ByteBufferPool{size: size}
	p.pool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 0, size)
			return &ByteBuffer{buf: buf, pool: p}
		},
	}
	return p
}

// Get 从池中获取一个 ByteBuffer。
func (p *ByteBufferPool) Get() *ByteBuffer {
	b := p.pool.Get().(*ByteBuffer)
	b.pool = p
	p.used.Add(1)
	return b
}

// Put 将一个 ByteBuffer 归还到池中。
func (p *ByteBufferPool) Put(b *ByteBuffer) {
	b.Reset()
	p.used.Add(-1)
	p.pool.Put(b)
}

// Used 返回当前从池中借出未归还的数量。
func (p *ByteBufferPool) Used() int64 {
	return p.used.Load()
}

// ---- 泛型对象池 ----

// GenericPool 是一个泛型对象池，支持任意类型的对象复用。
type GenericPool[T any] struct {
	pool     sync.Pool
	factory  func() T
	resetter func(T)
	used     atomic.Int64
}

// NewGenericPool 创建一个泛型对象池。
func NewGenericPool[T any](factory func() T, resetter func(T)) *GenericPool[T] {
	p := &GenericPool[T]{factory: factory, resetter: resetter}
	p.pool = sync.Pool{
		New: func() interface{} {
			return factory()
		},
	}
	return p
}

// Get 从池中获取一个对象。
func (p *GenericPool[T]) Get() T {
	obj := p.pool.Get().(T)
	p.used.Add(1)
	return obj
}

// Put 将对象归还到池中。
func (p *GenericPool[T]) Put(obj T) {
	if p.resetter != nil {
		p.resetter(obj)
	}
	p.used.Add(-1)
	p.pool.Put(obj)
}

// Used 返回当前借出未归还的数量。
func (p *GenericPool[T]) Used() int64 {
	return p.used.Load()
}

// ---- SIP 消息池 ----

// MessagePool 管理 SIP 消息对象的复用。
type MessagePool struct {
	rawBufPool *ByteBufferPool
	headerPool *ByteBufferPool
	bodyPool   *ByteBufferPool
}

// NewMessagePool 创建 SIP 消息池。
func NewMessagePool(rawBufSize, headerBufSize int) *MessagePool {
	return &MessagePool{
		rawBufPool: NewByteBufferPool(rawBufSize),
		headerPool: NewByteBufferPool(headerBufSize),
		bodyPool:   NewByteBufferPool(4096),
	}
}

// GetRawBuffer 获取原始消息缓冲区。
func (mp *MessagePool) GetRawBuffer() *ByteBuffer {
	return mp.rawBufPool.Get()
}

// PutRawBuffer 归还原始消息缓冲区。
func (mp *MessagePool) PutRawBuffer(b *ByteBuffer) {
	mp.rawBufPool.Put(b)
}

// GetHeaderBuffer 获取头域缓冲区。
func (mp *MessagePool) GetHeaderBuffer() *ByteBuffer {
	return mp.headerPool.Get()
}

// PutHeaderBuffer 归还头域缓冲区。
func (mp *MessagePool) PutHeaderBuffer(b *ByteBuffer) {
	mp.headerPool.Put(b)
}

// GetBodyBuffer 获取消息体缓冲区。
func (mp *MessagePool) GetBodyBuffer() *ByteBuffer {
	return mp.bodyPool.Get()
}

// PutBodyBuffer 归还消息体缓冲区。
func (mp *MessagePool) PutBodyBuffer(b *ByteBuffer) {
	mp.bodyPool.Put(b)
}

// ---- 零拷贝缓冲区 ----

// ZeroCopyBuffer 零拷贝缓冲区，引用计数管理生命周期。
type ZeroCopyBuffer struct {
	data []byte
	refs atomic.Int32
	pool *ZeroCopyPool
}

// Data 返回缓冲区数据（只读）。
func (z *ZeroCopyBuffer) Data() []byte {
	return z.data
}

// Retain 增加引用计数。
func (z *ZeroCopyBuffer) Retain() {
	z.refs.Add(1)
}

// Release 减少引用计数，归零时归还到池。
func (z *ZeroCopyBuffer) Release() {
	if z.refs.Add(-1) <= 0 {
		if z.pool != nil {
			z.pool.put(z)
		}
	}
}

// Refs 返回当前引用计数。
func (z *ZeroCopyBuffer) Refs() int32 {
	return z.refs.Load()
}

// ZeroCopyPool 零拷贝缓冲区池。
type ZeroCopyPool struct {
	tiered *TieredPool
}

// NewZeroCopyPool 创建零拷贝缓冲区池。
func NewZeroCopyPool() *ZeroCopyPool {
	return &ZeroCopyPool{
		tiered: NewTieredPool(),
	}
}

// Get 获取指定大小的零拷贝缓冲区，引用计数初始为 1。
func (zp *ZeroCopyPool) Get(size int) *ZeroCopyBuffer {
	buf := zp.tiered.Get(size)
	zcb := &ZeroCopyBuffer{
		data: buf[:size],
		pool: zp,
	}
	zcb.refs.Store(1)
	return zcb
}

func (zp *ZeroCopyPool) put(z *ZeroCopyBuffer) {
	zp.tiered.Put(z.data)
	z.data = nil
}

// ---- 全局默认池实例 ----

var (
	// DefaultTieredPool 全局分层内存池。
	DefaultTieredPool = NewTieredPool()
	// DefaultMessagePool 全局 SIP 消息池。
	DefaultMessagePool = NewMessagePool(2048, 1024)
	// DefaultZeroCopyPool 全局零拷贝池。
	DefaultZeroCopyPool = NewZeroCopyPool()
)

// GetBuffer 从全局分层池获取缓冲区。
func GetBuffer(size int) []byte {
	return DefaultTieredPool.Get(size)
}

// PutBuffer 归还缓冲区到全局分层池。
func PutBuffer(buf []byte) {
	DefaultTieredPool.Put(buf)
}

// 确保 math 被使用
var _ = math.MaxInt32
