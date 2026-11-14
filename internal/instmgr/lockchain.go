// @author: kevinkelly
// @license: Apache Licence
// @file: lockchain.go
// @time: 2026/10/28

package instmgr

// LockNode 是阻塞链上的一个线程。
type LockNode struct {
	Thread int64  `json:"thread"`
	Depth  int    `json:"depth"`  // 0 是根因，往下每一层是被上一层阻塞的线程
	Role   string `json:"role"`   // root 根因 / blocked 被阻塞
	SQL    string `json:"sql"`    // 该线程当前（或最近）的语句；空闲事务可能为空
	Secs   int64  `json:"secs"`   // root：事务已持续秒数；blocked：已等待秒数
	Object string `json:"object"` // blocked：等待的对象
}

// LockChain 是一条阻塞链：Nodes 按深度优先排好，第一个是根因。
type LockChain struct {
	Root  int64      `json:"root"`
	Nodes []LockNode `json:"nodes"`
}

// BuildLockChains 把“等待者 - 持有者”的成对记录整理成阻塞链：
// 根因是阻塞别人、自己却没在等别人的线程；被它阻塞的线程挂在它下面，被阻塞者又阻塞别人就继续往下挂。
// 出现环（互相等待，通常已被死锁检测处理，但采样瞬间可能看到）时，环里取第一个出现的持有者当“根”，保证每个线程只出现一次。
func BuildLockChains(rows []LockRow) []LockChain {
	if len(rows) == 0 {
		return []LockChain{}
	}
	waiting := map[int64]bool{}
	children := map[int64][]LockRow{}
	var blockers []int64
	seenBlocker := map[int64]bool{}
	for _, r := range rows {
		waiting[r.WaitThread] = true
		children[r.BlockThread] = append(children[r.BlockThread], r)
		if !seenBlocker[r.BlockThread] {
			seenBlocker[r.BlockThread] = true
			blockers = append(blockers, r.BlockThread)
		}
	}
	visited := map[int64]bool{}
	out := []LockChain{}

	var walk func(id int64, depth int, via *LockRow, chain *LockChain)
	walk = func(id int64, depth int, via *LockRow, chain *LockChain) {
		n := LockNode{Thread: id, Depth: depth, Role: "blocked"}
		if via != nil {
			n.SQL, n.Secs, n.Object = via.WaitSQL, via.WaitSecs, via.WaitObject
		} else {
			n.Role = "root"
			for _, r := range children[id] {
				if n.SQL == "" {
					n.SQL = r.BlockSQL
				}
				if r.BlockSecs > n.Secs {
					n.Secs = r.BlockSecs
				}
			}
		}
		chain.Nodes = append(chain.Nodes, n)
		for _, r := range children[id] {
			r := r
			if visited[r.WaitThread] {
				continue
			}
			visited[r.WaitThread] = true
			walk(r.WaitThread, depth+1, &r, chain)
		}
	}
	start := func(id int64) {
		visited[id] = true
		ch := LockChain{Root: id}
		walk(id, 0, nil, &ch)
		out = append(out, ch)
	}
	for _, b := range blockers {
		if !waiting[b] && !visited[b] {
			start(b)
		}
	}
	for _, b := range blockers { // 剩下的是环
		if !visited[b] {
			start(b)
		}
	}
	return out
}
