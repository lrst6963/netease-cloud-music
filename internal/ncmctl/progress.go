package ncmctl

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// displayWidth 计算字符串的显示宽度（中文字符计为2，其他计为1）
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if r > 127 {
			width += 2
		} else {
			width += 1
		}
	}
	return width
}

// truncateByWidth 根据显示宽度截断字符串
func truncateByWidth(s string, maxWidth int) string {
	if displayWidth(s) <= maxWidth {
		return s
	}

	targetWidth := maxWidth - 2
	currentWidth := 0
	runes := []rune(s)
	for i, r := range runes {
		w := 1
		if r > 127 {
			w = 2
		}
		if currentWidth+w > targetWidth {
			return string(runes[:i]) + ".."
		}
		currentWidth += w
	}
	return s
}

// ProgressTracker 进度追踪器
type ProgressTracker struct {
	Total    int64
	Current  int64
	Filename string
	Order    int
}

func (pt *ProgressTracker) Add(n int) {
	atomic.AddInt64(&pt.Current, int64(n))
}

func (pt *ProgressTracker) String(termWidth int) string {
	current := atomic.LoadInt64(&pt.Current)

	// 计算进度条可用宽度
	// 格式: Name(35) + " ["(2) + Bar(N) + "] "(2) + Percent(4) = 43 + N
	const nameWidth = 35
	const fixedOverhead = nameWidth + 2 + 2 + 4

	// 确保终端宽度足够显示基本信息
	width := termWidth - fixedOverhead - 1
	if width < 10 {
		width = 10
	}

	percent := 0.0
	if pt.Total > 0 {
		percent = float64(current) / float64(pt.Total) * 100
	}

	// 计算进度条位置
	pos := int(percent / 100 * float64(width))
	if pos >= width {
		pos = width - 1
	}
	if pos < 0 {
		pos = 0
	}

	left := strings.Repeat("▇", pos)

	mouth := ">"
	if pos%2 != 0 {
		mouth = "<"
	}
	// 完成时
	if current >= pt.Total {
		mouth = "▇"
	}

	right := strings.Repeat("·", width-pos-1)

	// 文件名显示宽度
	displayName := truncateByWidth(pt.Filename, nameWidth)

	// 计算填充空格
	paddingLen := nameWidth - displayWidth(displayName)
	if paddingLen < 0 {
		paddingLen = 0
	}
	padding := strings.Repeat(" ", paddingLen)

	return fmt.Sprintf("%s%s [%s%s%s] %3.0f%%", displayName, padding, left, mouth, right, percent)
}

// ProgressManager 进度管理器
type ProgressManager struct {
	trackers []*ProgressTracker
	mu       sync.Mutex
	stop     chan struct{}
	logs     chan string
	running  bool
}

func NewProgressManager() *ProgressManager {
	return &ProgressManager{
		trackers: make([]*ProgressTracker, 0),
	}
}

func (m *ProgressManager) Log(msg string) {
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()

	if running {
		m.logs <- msg
	} else {
		fmt.Println(msg)
	}
}

func (m *ProgressManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return
	}
	m.running = true
	m.stop = make(chan struct{})
	m.logs = make(chan string, 100)
	go m.run()
}

func (m *ProgressManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return
	}
	close(m.stop)
	m.running = false
}

func (m *ProgressManager) Add(pt *ProgressTracker) {
	m.mu.Lock()
	m.trackers = append(m.trackers, pt)
	m.mu.Unlock()
	m.Start()
}

// Finish 从管理器中移除并打印最终状态（如果完成）
func (m *ProgressManager) Finish(pt *ProgressTracker) {
	m.mu.Lock()

	removed := false
	for i, t := range m.trackers {
		if t == pt {
			m.trackers = append(m.trackers[:i], m.trackers[i+1:]...)
			removed = true
			break
		}
	}
	running := m.running
	m.mu.Unlock()

	if removed && running {
		// 获取终端宽度
		fd := int(os.Stdout.Fd())
		width, _, err := term.GetSize(fd)
		if err != nil || width <= 0 {
			width = 80
		}
		// 生成最终字符串并通过 Log 打印，达到"固化"效果
		m.Log(pt.String(width))
	} else if removed && !running {
		// 假如没有运行，直接打印
		fd := int(os.Stdout.Fd())
		width, _, err := term.GetSize(fd)
		if err != nil || width <= 0 {
			width = 80
		}
		fmt.Println(pt.String(width))
	}

	// 检查是否需要停止
	m.mu.Lock()
	if len(m.trackers) == 0 && m.running {
		close(m.stop)
		m.running = false
	}
	m.mu.Unlock()
}

func (m *ProgressManager) Remove(pt *ProgressTracker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, t := range m.trackers {
		if t == pt {
			m.trackers = append(m.trackers[:i], m.trackers[i+1:]...)
			break
		}
	}
	if len(m.trackers) == 0 {
		if m.running {
			close(m.stop)
			m.running = false
		}
	}
}

// printTrackers 绘制所有进度条并返回绘制的行数
func (m *ProgressManager) printTrackers(lastLines int) int {
	m.mu.Lock()
	trackers := make([]*ProgressTracker, len(m.trackers))
	copy(trackers, m.trackers)
	m.mu.Unlock()

	if len(trackers) == 0 {
		return 0
	}

	// 排序
	sort.Slice(trackers, func(i, j int) bool {
		return trackers[i].Order < trackers[j].Order
	})

	// Get term width
	fd := int(os.Stdout.Fd())
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		width = 80
	}

	for _, pt := range trackers {
		// 使用 \033[K 清除行内剩余内容，防止残留
		fmt.Printf("%s\033[K\n", pt.String(width))
	}
	// 如果新打印的行数少于旧行数，清除多余的行
	if len(trackers) < lastLines {
		diff := lastLines - len(trackers)
		for i := 0; i < diff; i++ {
			fmt.Printf("\033[K\n")
		}
		// 恢复光标位置
		fmt.Printf("\033[%dA", diff)
	}

	return len(trackers)
}

func (m *ProgressManager) run() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastLines := 0

	for {
		select {
		case <-m.stop:
			// 清除所有行
			if lastLines > 0 {
				fmt.Printf("\033[%dA\033[J", lastLines)
			}
			return
		case msg := <-m.logs:
			// 清除当前进度条
			if lastLines > 0 {
				fmt.Printf("\033[%dA\033[J", lastLines)
			}
			// 打印日志
			fmt.Println(msg)
			// 立即重新绘制进度条
			lastLines = m.printTrackers(0)
		case <-ticker.C:
			// Move cursor up
			if lastLines > 0 {
				fmt.Printf("\033[%dA", lastLines)
			}
			// Redraw
			lastLines = m.printTrackers(lastLines)
		}
	}
}

// ProgressWriter 适配 io.Writer
type ProgressWriter struct {
	Writer  io.Writer
	Tracker *ProgressTracker
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n, err := pw.Writer.Write(p)
	if err != nil {
		return n, err
	}
	pw.Tracker.Add(n)
	return n, nil
}
