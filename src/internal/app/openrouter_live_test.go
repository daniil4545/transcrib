//go:build live

package app

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOpenRouter_Live — живой вызов реального OpenRouter. Build-тег `live`: обычный
// go test его НЕ запускает (платный API не дёргается на каждом прогоне). Ключ/модель —
// из окружения (make грузит .env). Запуск: `make live-summary`.
func TestOpenRouter_Live(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	base := os.Getenv("OPENROUTER_BASE_URL")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		model = "google/gemini-2.5-flash"
	}

	transcript := "Спикер 1: Коллеги, давайте обсудим запуск transcrib. Что с интеграцией оплаты?\n" +
		"Спикер 2: ЮKassa почти готова, ждём подтверждения магазина. Думаю, к пятнице будет.\n" +
		"Спикер 1: Хорошо. Анна, возьми на себя тексты для лендинга, нужны к среде.\n" +
		"Спикер 3: Окей, сделаю до среды. Ещё нужно решить по триалу — три встречи или пять?\n" +
		"Спикер 1: Оставляем три, как договорились с заказчиком.\n" +
		"Спикер 2: Тогда я обновлю конфиг и выкачу на прод после подтверждения оплаты.\n" +
		"Спикер 1: Итог: оплата к пятнице, лендинг к среде, триал — три встречи."

	s := NewOpenRouterSummarizer(key, base, model, os.Getenv("OPENROUTER_PROXY_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	out, err := s.Summarize(ctx, transcript)
	if err != nil {
		t.Fatalf("live Summarize (model=%s): %v", model, err)
	}
	if out == "" {
		t.Fatal("empty summary")
	}
	t.Logf("model=%s\n--- SUMMARY ---\n%s\n--- END ---", model, out)
}
