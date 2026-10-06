package classify

import (
	"regexp"
	"strings"
	"unicode"
)

// Pattern matching cannot prevent injection, only flag the obvious attempts.
// Validation already blocks values outside the schema, so what an injection can
// still win is a wrong category or priority. Flagging makes that visible rather
// than silent. A rephrased attempt gets through, which is why the flag feeds a
// review queue and not a block list.

// injectionPatterns match text addressing the model rather than describing a
// problem. Narrow on purpose: "this is urgent" is a customer, "classify this as
// technical" is not.
var injectionPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"override", regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.]{0,40}\b(previous|prior|above|earlier|all)\b[^.]{0,20}\b(instruction|prompt|rule|direction)`)},
	{"role reassignment", regexp.MustCompile(`(?i)\byou are (now|no longer)\b|\bact as\b[^.]{0,30}\b(assistant|model|classifier)\b`)},
	{"new instructions", regexp.MustCompile(`(?i)\b(new|updated|revised|real)\s+(instruction|prompt|task|rule)s?\b`)},
	{"system prompt", regexp.MustCompile(`(?i)\b(system|developer)\s+(prompt|message|instruction)`)},
	{"dictates category", regexp.MustCompile(`(?i)\b(classify|categoris[ez]|categoriz[ez]|label|mark|tag)\b[^.]{0,30}\bas\b[^.]{0,30}\b(billing|technical|account|other)\b`)},
	{"dictates priority", regexp.MustCompile(`(?i)\b(priority|urgency)\b[^.]{0,20}\b(high|low|medium|critical|urgent)\b|\bset\b[^.]{0,20}\bpriority\b`)},
	{"dictates summary", regexp.MustCompile(`(?i)\bsummari[sz]e\b[^.]{0,30}\bas\b`)},
	{"output shaping", regexp.MustCompile(`(?i)\b(respond|reply|answer|output|return)\b[^.]{0,20}\b(only|exactly|with)\b[^.]{0,20}("|'|json|\{)`)},
}

// zeroWidth characters break a literal match while leaving the text looking
// identical, so they go before scanning.
var zeroWidth = regexp.MustCompile(`[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2064}\x{FEFF}]`)

// Suspicious reports whether a ticket is addressing the classifier, and names
// the patterns that matched. Reasons are logged and stored but never returned:
// telling a caller which rule caught them is an evasion guide.
func Suspicious(subject, body string) (bool, []string) {
	text := normalise(subject + "\n" + body)

	var reasons []string
	for _, p := range injectionPatterns {
		if p.re.MatchString(text) {
			reasons = append(reasons, p.name)
		}
	}
	return len(reasons) > 0, reasons
}

// normalise folds the cheap evasions: zero-width characters, whitespace runs,
// smart quotes and dashes.
func normalise(s string) string {
	s = zeroWidth.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '‘', '’', '‚', '‛':
			return '\''
		case '“', '”', '„', '‟':
			return '"'
		case '‐', '‑', '‒', '–', '—', '―':
			return '-'
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
