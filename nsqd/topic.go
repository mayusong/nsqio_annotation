package nsqd

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsqio/go-diskqueue"
	"github.com/nsqio/nsq/internal/lg"
	"github.com/nsqio/nsq/internal/quantile"
	"github.com/nsqio/nsq/internal/util"
)

type Topic struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	messageCount uint64
	messageBytes uint64

	sync.RWMutex

	name              string
	channelMap        map[string]*Channel
	backend           BackendQueue				//用于接收消息，消息存放到磁盘
	memoryMsgChan     chan *Message				//用于接收消息，消息存放到内存
	startChan         chan int					//用于阻塞messagePump，直到收到startChan信号
	exitChan          chan int					//退出的通道
	channelUpdateChan chan int					//channel有变更时发送通知的通道
	waitGroup         util.WaitGroupWrapper
	exitFlag          int32						//退出
	idFactory         *guidFactory

	ephemeral      bool							//是否是临时的topic
	deleteCallback func(*Topic)					//执行删除的函数
	deleter        sync.Once					//只执行一次

	paused    int32								//是否暂停  1 暂停，0不暂停
	pauseChan chan int							//发送暂停或取消暂停的消息

	ctx *context								//上下文，储存nsqd指针
}

// Topic constructor
//topic分为临时topic和永久topic,临时topic  backend变量使用newDummyBackendQueue函数初始化。该函数生成一个无任何功能的dummyBackendQueue结构；
//
//对于永久的topic，backend使用newDiskQueue函数返回diskQueue类型赋值，并开启新的goroutine来进行数据的持久化。
//
//1.初始化Topic结构体
//
//2.对于非临时topic，则初始化topic的backend为diskQueue,diskQueue是记录在磁盘文件中的FIFO队列（当内存队列满的时候会用到该磁盘队列)
//
//3.开启协程调用messagePump函数，messagePump的作用是将受到的队列（内存队列topic中的memoryMsgChan和磁盘队列disQueue）中的消息投递到topic关联的所有channel中
//
//4.通知nsqd新建了topic
func NewTopic(topicName string, ctx *context, deleteCallback func(*Topic)) *Topic {
	t := &Topic{
		name:              topicName,
		channelMap:        make(map[string]*Channel),
		memoryMsgChan:     make(chan *Message, ctx.nsqd.getOpts().MemQueueSize),
		startChan:         make(chan int, 1),
		exitChan:          make(chan int),
		channelUpdateChan: make(chan int),
		ctx:               ctx,
		paused:            0,
		pauseChan:         make(chan int),
		deleteCallback:    deleteCallback,
		idFactory:         NewGUIDFactory(ctx.nsqd.getOpts().ID),
	}
	//如果topic的名称以#ephemeral开头，如果是则是临时topic
	if strings.HasSuffix(topicName, "#ephemeral") {
		t.ephemeral = true
		t.backend = newDummyBackendQueue()
	} else {
		dqLogf := func(level diskqueue.LogLevel, f string, args ...interface{}) {
			opts := ctx.nsqd.getOpts()
			lg.Logf(opts.Logger, opts.LogLevel, lg.LogLevel(level), f, args...)
		}
		t.backend = diskqueue.New(
			topicName,
			ctx.nsqd.getOpts().DataPath,
			ctx.nsqd.getOpts().MaxBytesPerFile,
			int32(minValidMsgLength),									//每条消息的最小长度
			int32(ctx.nsqd.getOpts().MaxMsgSize)+minValidMsgLength,		//每条消息的最大长度
			ctx.nsqd.getOpts().SyncEvery,
			ctx.nsqd.getOpts().SyncTimeout,
			dqLogf,
		)
	}

	t.waitGroup.Wrap(t.messagePump)
	//通知nsqd新建了Topic
	t.ctx.nsqd.Notify(t)

	return t
}

func (t *Topic) Start() {
	select {
	case t.startChan <- 1:
	default:
	}
}

// Exiting returns a boolean indicating if this topic is closed/exiting
func (t *Topic) Exiting() bool {
	return atomic.LoadInt32(&t.exitFlag) == 1
}

// GetChannel performs a thread safe operation
// to return a pointer to a Channel object (potentially new)
// for the given Topic
func (t *Topic) GetChannel(channelName string) *Channel {
	t.Lock()
	channel, isNew := t.getOrCreateChannel(channelName)
	t.Unlock()

	if isNew {
		// update messagePump state
		select {
		case t.channelUpdateChan <- 1:
		case <-t.exitChan:
		}
	}

	return channel
}

// this expects the caller to handle locking
func (t *Topic) getOrCreateChannel(channelName string) (*Channel, bool) {
	channel, ok := t.channelMap[channelName]
	if !ok {
		deleteCallback := func(c *Channel) {
			t.DeleteExistingChannel(c.name)
		}
		channel = NewChannel(t.name, channelName, t.ctx, deleteCallback)
		t.channelMap[channelName] = channel
		t.ctx.nsqd.logf(LOG_INFO, "TOPIC(%s): new channel(%s)", t.name, channel.name)
		return channel, true
	}
	return channel, false
}

func (t *Topic) GetExistingChannel(channelName string) (*Channel, error) {
	t.RLock()
	defer t.RUnlock()
	channel, ok := t.channelMap[channelName]
	if !ok {
		return nil, errors.New("channel does not exist")
	}
	return channel, nil
}

// DeleteExistingChannel removes a channel from the topic only if it exists
func (t *Topic) DeleteExistingChannel(channelName string) error {
	t.Lock()
	channel, ok := t.channelMap[channelName]
	if !ok {
		t.Unlock()
		return errors.New("channel does not exist")
	}
	delete(t.channelMap, channelName)
	// not defered so that we can continue while the channel async closes
	numChannels := len(t.channelMap)
	t.Unlock()

	t.ctx.nsqd.logf(LOG_INFO, "TOPIC(%s): deleting channel %s", t.name, channel.name)

	// delete empties the channel before closing
	// (so that we dont leave any messages around)
	channel.Delete()

	// update messagePump state
	select {
	case t.channelUpdateChan <- 1:
	case <-t.exitChan:
	}

	if numChannels == 0 && t.ephemeral == true {
		go t.deleter.Do(func() { t.deleteCallback(t) })
	}

	return nil
}

// PutMessage writes a Message to the queue
func (t *Topic) PutMessage(m *Message) error {
	t.RLock()
	defer t.RUnlock()
	if atomic.LoadInt32(&t.exitFlag) == 1 {
		return errors.New("exiting")
	}
	err := t.put(m)
	if err != nil {
		return err
	}
	atomic.AddUint64(&t.messageCount, 1)
	atomic.AddUint64(&t.messageBytes, uint64(len(m.Body)))
	return nil
}

// PutMessages writes multiple Messages to the queue
func (t *Topic) PutMessages(msgs []*Message) error {
	t.RLock()
	defer t.RUnlock()
	if atomic.LoadInt32(&t.exitFlag) == 1 {
		return errors.New("exiting")
	}

	messageTotalBytes := 0

	for i, m := range msgs {
		err := t.put(m)
		if err != nil {
			atomic.AddUint64(&t.messageCount, uint64(i))
			atomic.AddUint64(&t.messageBytes, uint64(messageTotalBytes))
			return err
		}
		messageTotalBytes += len(m.Body)
	}

	atomic.AddUint64(&t.messageBytes, uint64(messageTotalBytes))
	atomic.AddUint64(&t.messageCount, uint64(len(msgs)))
	return nil
}
//topic.go文件中的put函数，是topic中消息的获取来源
//
//    上面也提到topic中的消息存在topic中的memoryMsgChan队列中，该队列的默认长度是10000，如果该队列满了，则会将消息存到disQueue磁盘文件中
//
//当生产者PUB消息时，会根据topic的名称，将消息写入到对应topic的队列中
//
//1.如果memoryMsgChan队列没满，则将消息写入该队列
//
//2.如果满了则将消息写入到磁盘中
//
//(1)通过bufferPoolGet函数从buffer池中获取一个buffer，bufferPoolGet及以下bufferPoolPut函数是对sync.Pool的简单包装。 两个函数位于nsqd/buffer_pool.go中。
//
//(2）调用writeMessageToBackend函数将消息写入磁盘。
//
//(3)通过bufferPoolPut函数将buffer归还buffer池。
//
//(4)调用SetHealth函数将writeMessageToBackend的返回值写入errValue变量。 该变量衍生出IsHealthy，GetError和GetHealth3个函数，主要用于测试以及从HTTP API获取nsqd的运行情况（是否发生错误）
func (t *Topic) put(m *Message) error {
	select {
	case t.memoryMsgChan <- m:
	default:
		b := bufferPoolGet()
		err := writeMessageToBackend(b, m, t.backend)
		bufferPoolPut(b)
		t.ctx.nsqd.SetHealth(err)
		if err != nil {
			t.ctx.nsqd.logf(LOG_ERROR,
				"TOPIC(%s) ERROR: failed to write message to backend - %s",
				t.name, err)
			return err
		}
	}
	return nil
}

func (t *Topic) Depth() int64 {
	return int64(len(t.memoryMsgChan)) + t.backend.Depth()
}

// messagePump selects over the in-memory and backend queue and
// writes messages to every channel for this topic
//messagePump函数主要作用轮询将内存队列和磁盘队列中的消息投递给该topic关联的所有Channel，channel的更新，暂停或取消暂停及退出等
//
//1.不在Start()函数调用之前接收消息，即不跳出第一个for循环
//
//2.获取所有该topic对应的Channel
//
//3.当topic对应的Channel的数量大于0，并且该topic不是暂停状态时初始化memoryMsgChan和backendChan
//
//4.第二个for循环中的流程
//
//  (1)从内存队列或磁盘文件中获取消息，并投递给所有关联的Channel
//
//  (2)channel更新
//
//  (3)暂停或取消暂停
//
//  (4)退出  
func (t *Topic) messagePump() {
	var msg *Message
	var buf []byte
	var err error
	var chans []*Channel				//该topic对应的所有Channel
	var memoryMsgChan chan *Message		//内存消息队列
	var backendChan chan []byte			//backend队列

	// do not pass messages before Start(), but avoid blocking Pause() or GetChannel()
	//不在Start()函数调用之前接收消息
	for {
		select {
		case <-t.channelUpdateChan:		//channel update的消息通知
			continue
		case <-t.pauseChan:				//暂停
			continue
		case <-t.exitChan:				//退出
			goto exit
		case <-t.startChan:
		}
		break
	}
	t.RLock()
	for _, c := range t.channelMap {	//获取所有该topic对应的Channel
		chans = append(chans, c)
	}
	t.RUnlock()
	if len(chans) > 0 && !t.IsPaused() { 	//如果Channel的长度大于0，并且topic不是暂停状态
		memoryMsgChan = t.memoryMsgChan 	//获取内存队列
		backendChan = t.backend.ReadChan()	//获取backendChan
	}

	// main message loop
	for {
		select {
		case msg = <-memoryMsgChan:			 //从内存中获取
		case buf = <-backendChan:			 //从backendChan中获取
			msg, err = decodeMessage(buf)	 //需要将buf解码成msg
			if err != nil {
				t.ctx.nsqd.logf(LOG_ERROR, "failed to decode message - %s", err)
				continue
			}
		case <-t.channelUpdateChan:			 //channel更新
			chans = chans[:0]				 //重新获取chans
			t.RLock()
			for _, c := range t.channelMap {
				chans = append(chans, c)
			}
			t.RUnlock()
			//如果Channel的个数为0或者topic是暂停，则将memoryMsgChan和backendChan置为nil
			if len(chans) == 0 || t.IsPaused() {
				memoryMsgChan = nil
				backendChan = nil
			} else {		//负责重新指定memoryMsgChan和backendChan
				memoryMsgChan = t.memoryMsgChan
				backendChan = t.backend.ReadChan()
			}
			continue
			//暂停
		case <-t.pauseChan:
			//如果Channel的个数为0或者topic是暂停，则将memoryMsgChan和backendChan置为nil
			if len(chans) == 0 || t.IsPaused() {
				memoryMsgChan = nil
				backendChan = nil
			} else {		//负责重新指定memoryMsgChan和backendChan
				memoryMsgChan = t.memoryMsgChan
				backendChan = t.backend.ReadChan()
			}
			continue
		case <-t.exitChan:
			goto exit
		}
		//以下为处理收到的msg
		//遍历topic的所有的channel
		for i, channel := range chans {
			chanMsg := msg
			// copy the message because each channel
			// needs a unique instance but...
			// fastpath to avoid copy if its the first channel
			// (the topic already created the first copy)
			//复制消息，因为每个channel需要唯一的实例
			if i > 0 {
				chanMsg = NewMessage(msg.ID, msg.Body)
				chanMsg.Timestamp = msg.Timestamp
				chanMsg.deferred = msg.deferred
			}
			//发送延时消息
			if chanMsg.deferred != 0 {
				channel.PutMessageDeferred(chanMsg, chanMsg.deferred)
				continue
			}
			//发送即时消息
			err := channel.PutMessage(chanMsg)
			if err != nil {
				t.ctx.nsqd.logf(LOG_ERROR,
					"TOPIC(%s) ERROR: failed to put msg(%s) to channel(%s) - %s",
					t.name, msg.ID, channel.name, err)
			}
		}
	}

exit:
	t.ctx.nsqd.logf(LOG_INFO, "TOPIC(%s): closing ... messagePump", t.name)
}
//1.deleted为true，则通知nsqd
//
//2.close(t.exitChan)  退出memoryMsgChan函数
//
//3.如果deleted为true，则
//
//   (1)删除该topic对应的channel
//
//    (2)channel.Delete()清空队列中的消息并关闭退出
//
//   (3)t.Empty()清空内存队列和磁盘文件中的消息
//
//4.如果deleted为false，则
//
//   (1)关闭topic对应的channel
//
//   (2)调用t.flush()将内存队列memoryMsgChan中的消息写入到磁盘文件中
//
//    (3)关闭并退出disQueue（文件中的消息是不删除的）
//
//总结来说：
//
//对于删除操作，需要清空channelMap并删除所有channel，然后删除内存和磁盘中所有未投递的消息。最后关闭backend管理的的磁盘文件。
//
//对于关闭操作，不清空channelMap，只是关闭所有的channel，使用flush函数将所有memoryMsgChan中未投递的消息用writeMessageToBackend保存到磁盘中。最后关闭backend管理的的磁盘文件
// Delete empties the topic and all its channels and closes
func (t *Topic) Delete() error {
	return t.exit(true)
}

// Close persists all outstanding topic data and closes all its channels
func (t *Topic) Close() error {
	return t.exit(false)
}

func (t *Topic) exit(deleted bool) error {
	if !atomic.CompareAndSwapInt32(&t.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		t.ctx.nsqd.logf(LOG_INFO, "TOPIC(%s): deleting", t.name)

		// since we are explicitly deleting a topic (not just at system exit time)
		// de-register this from the lookupd
		t.ctx.nsqd.Notify(t)
	} else {
		t.ctx.nsqd.logf(LOG_INFO, "TOPIC(%s): closing", t.name)
	}

	close(t.exitChan)

	// synchronize the close of messagePump()
	t.waitGroup.Wait()

	if deleted {
		t.Lock()
		for _, channel := range t.channelMap {
			delete(t.channelMap, channel.name)
			channel.Delete()
		}
		t.Unlock()

		// empty the queue (deletes the backend files, too)
		t.Empty()
		return t.backend.Delete()
	}

	// close all the channels
	for _, channel := range t.channelMap {
		err := channel.Close()
		if err != nil {
			// we need to continue regardless of error to close all the channels
			t.ctx.nsqd.logf(LOG_ERROR, "channel(%s) close - %s", channel.name, err)
		}
	}

	// write anything leftover to disk
	t.flush()
	return t.backend.Close()
}

func (t *Topic) Empty() error {
	for {
		select {
		case <-t.memoryMsgChan:
		default:
			goto finish
		}
	}

finish:
	return t.backend.Empty()
}

func (t *Topic) flush() error {
	var msgBuf bytes.Buffer

	if len(t.memoryMsgChan) > 0 {
		t.ctx.nsqd.logf(LOG_INFO,
			"TOPIC(%s): flushing %d memory messages to backend",
			t.name, len(t.memoryMsgChan))
	}

	for {
		select {
		case msg := <-t.memoryMsgChan:
			err := writeMessageToBackend(&msgBuf, msg, t.backend)
			if err != nil {
				t.ctx.nsqd.logf(LOG_ERROR,
					"ERROR: failed to write message to backend - %s", err)
			}
		default:
			goto finish
		}
	}

finish:
	return nil
}

func (t *Topic) AggregateChannelE2eProcessingLatency() *quantile.Quantile {
	var latencyStream *quantile.Quantile
	t.RLock()
	realChannels := make([]*Channel, 0, len(t.channelMap))
	for _, c := range t.channelMap {
		realChannels = append(realChannels, c)
	}
	t.RUnlock()
	for _, c := range realChannels {
		if c.e2eProcessingLatencyStream == nil {
			continue
		}
		if latencyStream == nil {
			latencyStream = quantile.New(
				t.ctx.nsqd.getOpts().E2EProcessingLatencyWindowTime,
				t.ctx.nsqd.getOpts().E2EProcessingLatencyPercentiles)
		}
		latencyStream.Merge(c.e2eProcessingLatencyStream)
	}
	return latencyStream
}
//topic的暂停和取消暂停主要是通过原子操作topic中的paused字段来实现的，paused的值为1则是暂停，0是非暂停状态
func (t *Topic) Pause() error {
	return t.doPause(true)
}

func (t *Topic) UnPause() error {
	return t.doPause(false)
}

func (t *Topic) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&t.paused, 1)
	} else {
		atomic.StoreInt32(&t.paused, 0)
	}

	select {
	case t.pauseChan <- 1:
	case <-t.exitChan:
	}

	return nil
}

func (t *Topic) IsPaused() bool {
	return atomic.LoadInt32(&t.paused) == 1
}

func (t *Topic) GenerateID() MessageID {
retry:
	id, err := t.idFactory.NewGUID()
	if err != nil {
		time.Sleep(time.Millisecond)
		goto retry
	}
	return id.Hex()
}
