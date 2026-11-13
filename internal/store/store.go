// Package store 把每个节点每分钟的聚合数据写入本地文件，并按保留天数清理。
//
// 文件布局：<dir>/<nodeID>/<YYYYMMDD>.bin（按本地时区分天），
// 每条记录 24 字节定长：
//
//	int64   分钟起始 Unix 秒
//	uint16  发送次数
//	uint16  成功次数
//	float32 最小延迟 ms
//	float32 平均延迟 ms
//	float32 最大延迟 ms
package store

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const recordSize = 24

// Minute 是一分钟的聚合数据。
type Minute struct {
	T    int64   `json:"t"` // 分钟起始 Unix 秒
	Sent int     `json:"sent"`
	Recv int     `json:"recv"`
	Min  float64 `json:"min"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
}

// Store 历史数据存储。
type Store struct {
	dir string
	mu  sync.Mutex
}

// New 创建存储，dir 不存在时自动创建。
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func dayName(t time.Time) string { return t.Format("20060102") + ".bin" }

func encode(m Minute) []byte {
	b := make([]byte, recordSize)
	binary.LittleEndian.PutUint64(b[0:], uint64(m.T))
	binary.LittleEndian.PutUint16(b[8:], uint16(clampInt(m.Sent)))
	binary.LittleEndian.PutUint16(b[10:], uint16(clampInt(m.Recv)))
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(float32(m.Min)))
	binary.LittleEndian.PutUint32(b[16:], math.Float32bits(float32(m.Avg)))
	binary.LittleEndian.PutUint32(b[20:], math.Float32bits(float32(m.Max)))
	return b
}

func clampInt(v int) int {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return v
}

func decode(b []byte) Minute {
	return Minute{
		T:    int64(binary.LittleEndian.Uint64(b[0:])),
		Sent: int(binary.LittleEndian.Uint16(b[8:])),
		Recv: int(binary.LittleEndian.Uint16(b[10:])),
		Min:  float64(math.Float32frombits(binary.LittleEndian.Uint32(b[12:]))),
		Avg:  float64(math.Float32frombits(binary.LittleEndian.Uint32(b[16:]))),
		Max:  float64(math.Float32frombits(binary.LittleEndian.Uint32(b[20:]))),
	}
}

// Append 追加一条分钟记录。
func (s *Store) Append(id string, m Minute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, dayName(time.Unix(m.T, 0))), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(encode(m))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Query 读取 [from, to) 区间（Unix 秒）的分钟记录，按时间升序。
func (s *Store) Query(id string, from, to int64) ([]Minute, error) {
	var out []Minute
	start := time.Unix(from, 0)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	end := time.Unix(to, 0)
	for !day.After(end) {
		data, err := os.ReadFile(filepath.Join(s.dir, id, dayName(day)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for i := 0; i+recordSize <= len(data); i += recordSize {
			m := decode(data[i : i+recordSize])
			if m.T >= from && m.T < to {
				out = append(out, m)
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}

// Cleanup 删除超过 keepDays 天的文件，以及清理后变空的节点目录。
func (s *Store) Cleanup(keepDays int) (removed int, err error) {
	now := time.Now()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -keepDays)
	cut := cutoff.Format("20060102")
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.dir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		left := 0
		for _, f := range files {
			name := f.Name()
			if strings.HasSuffix(name, ".bin") && len(name) == 12 && name[:8] < cut {
				if os.Remove(filepath.Join(dir, name)) == nil {
					removed++
					continue
				}
			}
			left++
		}
		if left == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed, nil
}
