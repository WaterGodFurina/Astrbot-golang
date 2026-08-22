package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron expression support. Accepts 5 fields
// (minute hour day-of-month month day-of-week) or 6 fields
// (second minute hour day-of-month month day-of-week), matching Python
// croniter semantics where the leading field is seconds.
//
// Supported per field:
//   - `*`                any value
//   - `*/step`           every `step`
//   - `a-b`              inclusive range
//   - `a-b/step`         range with step
//   - `a,b,c`            list
//   - single value
//   - month/dow names: jan..dec, sun..sat (dow: 0=sun or 7=sun)

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// fieldSpec is one cron field (minute, hour, dom, month, dow).
type fieldSpec struct {
	any      bool
	values   map[int]bool
	step     int
	hasStep  bool
	from, to int
	rangeAny bool
}

// cronSchedule is a parsed cron expression. sec is nil for 5-field (minute
// granularity) expressions and set for 6-field (seconds granularity) ones.
type cronSchedule struct {
	sec                           *fieldSpec
	minute, hour, dom, month, dow *fieldSpec
}

// ParseRunAt parses a one-time execution datetime in a lenient, Python
// fromisoformat-like manner. Accepts RFC3339, "YYYY-MM-DDTHH:MM[:SS]", and
// space-separated variants. Naive (timezone-less) inputs are interpreted in
// the server's local timezone, matching Python's naive-datetime handling.
func ParseRunAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty run_at")
	}
	tzLayouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05 -07:00",
	}
	for _, layout := range tzLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	naiveLayouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range naiveLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("run_at must be ISO datetime, e.g. 2026-02-02T08:00:00+08:00")
}

// ParseCron parses a 5-field or 6-field cron expression. A 6-field expression
// is seconds-first (croniter style): "sec min hour dom mon dow".
func ParseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 && len(fields) != 6 {
		return nil, fmt.Errorf("cron expression must have 5 or 6 fields, got %d: %q", len(fields), expr)
	}
	s := &cronSchedule{}
	var err error
	off := 0
	if len(fields) == 6 {
		if s.sec, err = parseField(fields[0], 0, 59, nil); err != nil {
			return nil, fmt.Errorf("second: %w", err)
		}
		off = 1
	}
	if s.minute, err = parseField(fields[off], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = parseField(fields[off+1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = parseField(fields[off+2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day-of-month: %w", err)
	}
	if s.month, err = parseField(fields[off+3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = parseField(fields[off+4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("day-of-week: %w", err)
	}
	// Normalize dow 7 -> 0 (Sunday) so the spec is consistent.
	if s.dow.values != nil && s.dow.values[7] {
		s.dow.values[0] = true
	}
	if s.dow.rangeAny && s.dow.to == 7 {
		s.dow.to = 0
	}
	return s, nil
}

// parseField parses one cron field. names maps month/dow names to numbers.
func parseField(field string, min, max int, names map[string]int) (*fieldSpec, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, fmt.Errorf("empty field")
	}
	// A field may be a comma list; if any element is a range/step, expand here.
	parts := strings.Split(field, ",")
	spec := &fieldSpec{values: map[int]bool{}}
	hadValue := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty list element in %q", field)
		}
		if err := applyFieldPart(spec, part, min, max, names); err != nil {
			return nil, err
		}
		hadValue = true
	}
	if !hadValue {
		return nil, fmt.Errorf("empty field")
	}
	return spec, nil
}

func applyFieldPart(spec *fieldSpec, part string, min, max int, names map[string]int) error {
	step := 1
	stepPart := ""
	if i := strings.Index(part, "/"); i >= 0 {
		stepPart = part[i+1:]
		part = part[:i]
		if stepPart == "" {
			return fmt.Errorf("empty step in %q", part+"/")
		}
		var err error
		step, err = strconv.Atoi(stepPart)
		if err != nil || step <= 0 {
			return fmt.Errorf("invalid step %q", stepPart)
		}
	}
	if part == "*" {
		if step > 1 {
			spec.any = true
			spec.hasStep = true
			spec.step = step
			for v := min; v <= max; v++ {
				if (v-min)%step == 0 {
					spec.values[v] = true
				}
			}
		} else {
			spec.any = true
			for v := min; v <= max; v++ {
				spec.values[v] = true
			}
		}
		return nil
	}
	if i := strings.Index(part, "-"); i >= 0 {
		fromStr := part[:i]
		toStr := part[i+1:]
		from, err := parseFieldValue(fromStr, min, max, names)
		if err != nil {
			return err
		}
		to, err := parseFieldValue(toStr, min, max, names)
		if err != nil {
			return err
		}
		if from > to {
			// croniter 对 from>to 的区间做环绕匹配（如 dow 的 fri-mon）；本实现
			// 不支持环绕，静默交换会静默改变触发语义，告警提示用户。
			logger.I18nWarn("cron 字段 %q 的区间 %s-%s 起点大于终点，已交换处理（不支持环绕匹配）", part, fromStr, toStr)
			from, to = to, from
		}
		spec.rangeAny = true
		spec.from, spec.to = from, to
		for v := from; v <= to; v++ {
			if (v-from)%step == 0 {
				spec.values[v] = true
			}
		}
		return nil
	}
	v, err := parseFieldValue(part, min, max, names)
	if err != nil {
		return err
	}
	if step > 1 {
		// croniter 语义：单值带步长等价于 v..max/step（如 `5/15` → 5,20,35,50）。
		spec.rangeAny = true
		spec.from, spec.to = v, max
		for x := v; x <= max; x += step {
			spec.values[x] = true
		}
		return nil
	}
	spec.values[v] = true
	return nil
}

func parseFieldValue(s string, min, max int, names map[string]int) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if names != nil {
		if v, ok := names[s]; ok {
			if v < min || v > max {
				return 0, fmt.Errorf("value %q out of range [%d,%d]", s, min, max)
			}
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", v, min, max)
	}
	return v, nil
}

func (f *fieldSpec) matches(v int) bool {
	// Range values are expanded into f.values during parse, so lookup only.
	return f.values[v]
}

// dayMatches applies cron's day-of-month / day-of-week union-or-and semantics.
func (s *cronSchedule) dayMatches(d int, weekday time.Weekday) bool {
	domMatch := s.dom.matches(d)
	dowMatch := s.dow.matches(int(weekday))
	switch {
	case s.dom.any && !s.dow.any:
		return dowMatch
	case !s.dom.any && s.dow.any:
		return domMatch
	case s.dom.any && s.dow.any:
		return true
	}
	return domMatch || dowMatch
}

// Next returns the next time after `after` matching the schedule (in the same
// location as `after`).
func (s *cronSchedule) Next(after time.Time) time.Time {
	loc := after.Location()
	if s.sec != nil {
		return s.nextSeconds(after, loc)
	}
	t := after.Truncate(time.Minute).Add(time.Minute)
	// Safety bound: ~5 years of per-minute iterations (never reached for a
	// valid schedule; time.Date normalizes overflowing fields).
	for i := 0; i < 366*24*60*5; i++ {
		y, m, d := t.Date()
		h := t.Hour()
		mn := t.Minute()
		if !s.month.matches(int(m)) {
			t = time.Date(y, time.Month(int(m)+1), 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(d, t.Weekday()) {
			t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.hour.matches(h) {
			t = time.Date(y, m, d, h+1, 0, 0, 0, loc)
			continue
		}
		if !s.minute.matches(mn) {
			t = time.Date(y, m, d, h, mn+1, 0, 0, loc)
			continue
		}
		return t
	}
	return time.Time{}
}

// nextSeconds computes the next fire time for a 6-field (seconds) schedule,
// iterating at second granularity. Jumps keep the iteration count tiny even
// though the safety bound spans multiple years.
func (s *cronSchedule) nextSeconds(after time.Time, loc *time.Location) time.Time {
	t := after.Truncate(time.Second).Add(time.Second)
	for i := 0; i < 366*24*60*60*3; i++ {
		y, m, d := t.Date()
		h := t.Hour()
		mn := t.Minute()
		sc := t.Second()
		if !s.month.matches(int(m)) {
			t = time.Date(y, time.Month(int(m)+1), 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(d, t.Weekday()) {
			t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.hour.matches(h) {
			t = time.Date(y, m, d, h+1, 0, 0, 0, loc)
			continue
		}
		if !s.minute.matches(mn) {
			t = time.Date(y, m, d, h, mn+1, 0, 0, loc)
			continue
		}
		if !s.sec.matches(sc) {
			t = time.Date(y, m, d, h, mn, sc+1, 0, loc)
			continue
		}
		return t
	}
	return time.Time{}
}
