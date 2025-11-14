package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/exp/slog"

	"url-shortener/internal/config"
	"url-shortener/internal/http-server/handlers/redirect"
	"url-shortener/internal/http-server/handlers/url/save"
	mwLogger "url-shortener/internal/http-server/middleware/logger"
	"url-shortener/internal/lib/logger/handlers/slogpretty"
	"url-shortener/internal/lib/logger/sl"
	"url-shortener/internal/storage/sqlite"
)

const (
	envLocal = "local"
	envDev   = "dev"
	envProd  = "prod"
)

func main() {
	cfg := config.MustLoad()

	log := setupLogger(cfg.Env)

	log.Info(
		"starting url-shortener",
		slog.String("env", cfg.Env),
		slog.String("version", "123"),
	)
	log.Debug("debug messages are enabled")

	storage, err := sqlite.New(cfg.StoragePath)
	if err != nil {
		log.Error("failed to init storage", sl.Err(err))
		os.Exit(1)
	}

	router := chi.NewRouter()

	router.Use(middleware.RequestID)
	router.Use(middleware.Logger)
	router.Use(mwLogger.New(log))
	router.Use(middleware.Recoverer)
	router.Use(middleware.URLFormat)

	router.Route("/url", func(r chi.Router) {
		r.Use(middleware.BasicAuth("url-shortener", map[string]string{
			cfg.HTTPServer.User: cfg.HTTPServer.Password,
		}))

		r.Post("/", save.New(log, storage))
		// TODO: add DELETE /url/{id}
	})

	router.Get("/{alias}", redirect.New(log, storage))

	log.Info("starting server", slog.String("address", cfg.Address))

	// ❗graceful shutdown

	// Анализ  от google:
	// 1️⃣ Инициализация канала сигналов (done)
	// done: Это наш "стоп-кран".
	// Это буферизованный канал, который будет ожидать системные сигналы.
	done := make(chan os.Signal, 1)
	// signal.Notify: Регистрирует канал done для получения уведомлений,
	// когда операционная система отправляет сигналы прерывания (Ctrl+C),
	// SIGINT или SIGTERM (используется в Docker, Kubernetes, systemd для завершения процессов).
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// 2️⃣ Конфигурация и запуск сервера
	// http.Server: Сервер корректно сконфигурирован с таймаутами для чтения/записи,
	// что очень важно для продакшена.
	srv := &http.Server{
		Addr:         cfg.Address,
		Handler:      router,
		ReadTimeout:  cfg.HTTPServer.Timeout,
		WriteTimeout: cfg.HTTPServer.Timeout,
		IdleTimeout:  cfg.HTTPServer.IdleTimeout,
	}

	// Отдельная горутина: Сервер запускается в своей собственной горутине.
	// Это необходимо, так как ListenAndServe() является блокирующим вызовом.
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Error("failed to start server")
		}
	}()

	log.Info("server started")

	// 3️⃣ Ожидание сигнала остановки
	// <-done: Это критическая точка синхронизации. Основная горутина main блокируется здесь.
	// Она будет ждать, пока в канал done не придет системный сигнал.
	// Как только пользователь нажимает Ctrl+C, канал разблокируется, и выполнение продолжается.
	<-done
	log.Info("stopping server")

	// 4️⃣ Корректное завершение с таймаутом (Shutdown и context.WithTimeout)
	// context.WithTimeout: Создает контекст, который автоматически отменится через 10 секунд.
	// Это наша "страховка" от зависания сервера.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// TODO: move timeout to config

	// Всегда нужно отменять контекст, чтобы освободить его ресурсы
	defer cancel()

	// srv.Shutdown(ctx): Вызывает изящное (graceful) завершение работы.
	// Он перестает принимать новые запросы, но дает активным запросам время завершиться.
	// Он использует канал <-ctx.Done() (который находится внутри ctx), чтобы узнать, когда истечет 10-секундный лимит.
	if err := srv.Shutdown(ctx); err != nil {
		// Обработка ошибок: Если Shutdown возвращает ошибку
		// (обычно context deadline exceeded), это логируется.
		log.Error("failed to stop server", sl.Err(err))
		return
	}

	// TODO: close storage

	log.Info("server stopped")
}

func setupLogger(env string) *slog.Logger {
	var log *slog.Logger

	switch env {
	case envLocal:
		log = setupPrettySlog()
	case envDev:
		log = slog.New(
			slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}),
		)
	case envProd:
		log = slog.New(
			slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		)
	default: // If env config is invalid, set prod settings by default due to security
		log = slog.New(
			slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		)
	}

	return log
}

func setupPrettySlog() *slog.Logger {
	opts := slogpretty.PrettyHandlerOptions{
		SlogOpts: &slog.HandlerOptions{
			Level: slog.LevelDebug,
		},
	}

	handler := opts.NewPrettyHandler(os.Stdout)

	return slog.New(handler)
}
