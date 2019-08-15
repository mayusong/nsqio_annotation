package pqueue

import (
	"container/heap"
)

type Item struct {
	Value    interface{}	//储存的消息内容
	Priority int64			//优先级的时间点
	Index    int			//在切片中的索引值
}

// this is a priority queue as implemented by a min heap
// ie. the 0th element is the *lowest* value
//PriorityQueue实现了heap接口
type PriorityQueue []*Item

func New(capacity int) PriorityQueue {
	return make(PriorityQueue, 0, capacity)
}

func (pq PriorityQueue) Len() int {
	return len(pq)
}

func (pq PriorityQueue) Less(i, j int) bool {
	return pq[i].Priority < pq[j].Priority
}

func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].Index = i
	pq[j].Index = j
}

func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	c := cap(*pq)
	if n+1 > c {
		npq := make(PriorityQueue, n, c*2)
		copy(npq, *pq)
		*pq = npq
	}
	*pq = (*pq)[0 : n+1]
	item := x.(*Item)
	item.Index = n
	(*pq)[n] = item
}
//Pop函数用于取出队列中优先级最高的元素（即Priority值最小，也就是根部元素）
func (pq *PriorityQueue) Pop() interface{} {
	n := len(*pq)
	c := cap(*pq)
	if n < (c/2) && c > 25 {
		npq := make(PriorityQueue, n, c/2)
		copy(npq, *pq)
		*pq = npq
	}
	item := (*pq)[n-1]
	item.Index = -1
	*pq = (*pq)[0 : n-1]
	return item
}
//PeekAndShift(max int64) 用于判断根部的元素是否超过max，如果超过则返回nil，如果没有则返回并移除根部元素（根部元素是最小值）
//
//在实际的项目中Priority字段存的是时间戳，比如说5分钟之后投递本条消息，则Priority字段存的就是5分钟之后的时间戳。
//而PeekAndShift(max int64)中max值就是当前时间，如果队列中的根部元素大于当前时间戳max的值，则说明队列中没有可以投递的消息，故返回nil。
//如果小于等于，则根部元素存放的消息可以投递，就是就返回并移除该根部元素。
func (pq *PriorityQueue) PeekAndShift(max int64) (*Item, int64) {
	if pq.Len() == 0 {
		return nil, 0
	}

	item := (*pq)[0]
	if item.Priority > max {
		return nil, item.Priority - max
	}
	heap.Remove(pq, 0)

	return item, 0
}
