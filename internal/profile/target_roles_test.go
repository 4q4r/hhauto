package profile

// T93 (а): the deterministic «Целевые роли» parse — the shape follows the
// owner's live profile.md (labeled bullets, slash/comma lists, a negated
// exclude heading right after).

import (
	"reflect"
	"testing"
)

func TestTargetRolesFromProfileSection(t *testing.T) {
	text := `# Профиль кандидата

## Компенсация
- Цель: 150 000 ₽/мес net

## Целевые роли
- Разработка: fullstack / backend / Go / Python / Rust / Kotlin
- DevOps / DevSecOps
- Кодер (чистая разработка без менеджмента)
- QA (автотесты)
- Преподавание (программирование)
- Пентест / ИБ
- AI/LLM / агентные позиции

## НЕ целевые (exclude всегда)
- Аналитики (любые)
- Продакты / менеджеры проектов

## Ключевой опыт и проекты
- Go: продакшен
`
	want := []string{
		"fullstack", "backend", "Go", "Python", "Rust", "Kotlin",
		"DevOps", "DevSecOps",
		"Кодер (чистая разработка без менеджмента)",
		"QA (автотесты)",
		"Преподавание (программирование)",
		"Пентест", "ИБ",
		"AI", "LLM", "агентные позиции",
	}
	got := TargetRoles(text)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TargetRoles = %#v\nwant       = %#v", got, want)
	}
}

func TestTargetRolesAbsent(t *testing.T) {
	for name, text := range map[string]string{
		"no section":  "# Профиль\n\n## Компенсация\n- Цель: 150 000\n",
		"empty body":  "# Профиль\n\n## Целевые роли\n\n## Далее\n- x\n",
		"negated":     "## НЕ целевые роли\n- Аналитики\n",
		"empty input": "",
	} {
		if got := TargetRoles(text); got != nil {
			t.Fatalf("%s: TargetRoles = %#v, want nil", name, got)
		}
	}
}
