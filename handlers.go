package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type EvaluationResponse struct {
	FlagName string `json:"flag_name"`
	UserID   string `json:"user_id"`
	Result   bool   `json:"result"`
}

func (a *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *App) evaluationHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	ctx, span := a.Tracer.Start(r.Context(), "evaluate_feature_flag")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	userID := r.URL.Query().Get("user_id")
	flagName := r.URL.Query().Get("flag_name")

	if userID == "" || flagName == "" {
		a.EvaluationErrors.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("operation", "evaluate"),
				attribute.String("error_type", "missing_query_params"),
			),
		)

		a.EvaluationCounter.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("status", "bad_request"),
			),
		)

		a.EvaluationDuration.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(
				attribute.String("status", "bad_request"),
			),
		)

		span.SetAttributes(
			attribute.String("evaluation.status", "bad_request"),
		)

		http.Error(w, `{"error": "user_id e flag_name são obrigatórios"}`, http.StatusBadRequest)
		return
	}

	span.SetAttributes(
		attribute.String("feature_flag.name", flagName),
		attribute.String("user.id", userID),
	)

	result, err := a.getDecision(userID, flagName)

	if err != nil {
		if _, ok := err.(*NotFoundError); ok {
			result = false

			a.EvaluationCounter.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("status", "not_found_safe_false"),
					attribute.String("flag_name", flagName),
				),
			)

			span.SetAttributes(
				attribute.String("evaluation.status", "not_found_safe_false"),
				attribute.Bool("feature_flag.result", result),
			)
		} else {
			log.Printf("Erro ao avaliar flag '%s': %v", flagName, err)

			a.EvaluationErrors.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("operation", "evaluate"),
					attribute.String("error_type", "decision_error"),
					attribute.String("flag_name", flagName),
				),
			)

			a.EvaluationCounter.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("status", "error"),
					attribute.String("flag_name", flagName),
				),
			)

			a.EvaluationDuration.Record(ctx, time.Since(start).Seconds(),
				metric.WithAttributes(
					attribute.String("status", "error"),
					attribute.String("flag_name", flagName),
				),
			)

			span.RecordError(err)
			span.SetAttributes(
				attribute.String("evaluation.status", "error"),
			)

			http.Error(w, `{"error": "Erro interno ao avaliar a flag"}`, http.StatusBadGateway)
			return
		}
	}

	go func() {
		a.sendEvaluationEvent(userID, flagName, result)

		a.SqsEventsCounter.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("status", "sent_async"),
				attribute.String("flag_name", flagName),
			),
		)
	}()

	a.EvaluationCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("status", "success"),
			attribute.String("flag_name", flagName),
			attribute.Bool("result", result),
		),
	)

	a.EvaluationDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(
			attribute.String("status", "success"),
			attribute.String("flag_name", flagName),
		),
	)

	span.SetAttributes(
		attribute.String("evaluation.status", "success"),
		attribute.Bool("feature_flag.result", result),
	)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(EvaluationResponse{
		FlagName: flagName,
		UserID:   userID,
		Result:   result,
	})
}
