// Package stem reduces Russian and English words to snowball stems so FTS5 matches word forms
// ("запечатан" finds "запечатанный").
package stem

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball/english"
	"github.com/kljensen/snowball/russian"
)

// Tokens splits text into lowercase words of letters and digits.
func Tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Word stems one lowercase token. Mixed tokens (identifiers with digits) stay as they are.
func Word(w string) string {
	if len(w) < 3 {
		return w
	}
	cyr, lat := false, false
	for _, r := range w {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		case r < unicode.MaxASCII && unicode.IsLetter(r):
			lat = true
		case unicode.IsDigit(r):
			return w
		}
	}
	switch {
	case cyr && !lat:
		return russian.Stem(w, true)
	case lat && !cyr:
		return english.Stem(w, true)
	}
	return w
}

// Text returns the space-joined stems of every token in s.
func Text(s string) string {
	toks := Tokens(s)
	for i, t := range toks {
		toks[i] = Word(t)
	}
	return strings.Join(toks, " ")
}

// Query returns unique stems of s, dropping stop words, for building an FTS5 OR query.
func Query(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range Tokens(s) {
		if stop[t] {
			continue
		}
		st := Word(t)
		if len(st) < 2 || seen[st] {
			continue
		}
		seen[st] = true
		out = append(out, st)
	}
	return out
}

var stop = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`the a an and or of to in on for with is are was were be been it this that these those
	at by from as but not no do does did have has had i you we they he she my your our can could should would will
	what how why when where which who please just also about into than then there here so if
	и в во не что он на я с со как а то все она так его но да ты к у же вы за бы по только ее мне было
	вот от меня еще нет о из ему теперь когда даже ну вдруг ли если уже или ни быть был него до вас нибудь
	опять уж вам ведь там потом себя ничего ей может они тут где есть надо ней для мы тебя их чем была сам
	чтоб без будто чего раз тоже себе под будет ж тогда кто этот того потому этого какой совсем ним здесь
	этом один почти мой тем чтобы нее сейчас были куда зачем всех никогда можно при наконец два об другой
	хоть после над больше тот через эти нас про всего них какая много разве три эту моя впрочем хорошо
	свою этой перед иногда лучше чуть том нельзя такой им более всегда конечно всю между это давай сделай`) {
		m[w] = true
	}
	return m
}()
