package procstat

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// Stat identifies a Linux process and its state at the time of a /proc read.
type Stat struct {
	Comm       string
	State      byte
	Parent     int
	Group      int
	StartTicks uint64
}

func (s Stat) Exited() bool {
	return s.State == 'Z' || s.State == 'X' || s.State == 'x'
}

func Read(pid int) (Stat, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Stat{}, err
	}
	return Parse(data)
}

func Parse(data []byte) (Stat, error) {
	start := bytes.IndexByte(data, '(')
	end := bytes.LastIndexByte(data, ')')
	if start < 0 || end <= start || end+1 >= len(data) {
		return Stat{}, fmt.Errorf("malformed process stat")
	}
	// The comm field may contain parentheses. After its closing delimiter,
	// the first token is field 3 (state), and starttime is field 22.
	var result Stat
	i := 0
	for field := range bytes.FieldsSeq(data[end+1:]) {
		var err error
		switch i {
		case 0:
			if len(field) != 1 {
				return Stat{}, fmt.Errorf("invalid process state")
			}
			result.State = field[0]
		case 1:
			result.Parent, err = strconv.Atoi(string(field))
		case 2:
			result.Group, err = strconv.Atoi(string(field))
		case 19:
			result.StartTicks, err = strconv.ParseUint(string(field), 10, 64)
			if err == nil {
				result.Comm = string(data[start+1 : end])
				return result, nil
			}
		}
		if err != nil {
			return Stat{}, err
		}
		i++
	}
	return Stat{}, fmt.Errorf("short process stat")
}
