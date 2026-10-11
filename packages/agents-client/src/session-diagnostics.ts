import { canonicalUuid, exactFields, isRecord, sameResourceId } from "./response-projection";
import { invalidAdminResponse } from "./admin-projection";

export type NativeFailureCode = "authentication_error" | "rate_limit_exceeded" | "usage_limit_exceeded" | "server_overloaded" | "server_error" | "invalid_request" | "resource_not_found" | "request_timeout" | "context_length_exceeded" | "cyber_policy" | "connection_failed" | "provider_billing_unavailable";
export type DiagnosticFailureCode = NativeFailureCode | "harness_error" | "model_provider_required" | "runtime_unavailable" | "runtime_disconnected" | "runtime_preparation_failed" | "execution_interrupted" | "delivery_unconfirmed" | "input_rejected" | "executor_protocol_error" | "core_storage_failed" | "internal_error" | "environment_connection_timeout" | "environment_unavailable" | "environment_provisioning_failed";
export type ProvisioningFailureParams = { step: "setup" | "python" | "npm" | "system" | "file" | "skill" | null; index: number | null; exit_code: number | null };
export type ConnectionFailureParams = { http_status: number | null };
export type DiagnosticFailure = { code: DiagnosticFailureCode; params: Record<string, never> | ProvisioningFailureParams | ConnectionFailureParams; failed_at: string | null };
export type SessionDiagnosticFailure = DiagnosticFailure & { source: "turn" | "environment" | "environment_input"; turn_id?: string };
export type SessionDiagnostics = { object: "core.session_diagnostics"; session_id: string; status: "idle" | "in_progress" | "requires_action" | "failed"; failure: SessionDiagnosticFailure | null };
/** Core receipt intervals, distinct from the public tool-reported duration_ms. */
export type ItemDiagnosticTiming = { item_id: string; started_at: string; completed_at: string | null; observed_duration_ms: number | null };
export type TurnDiagnostics = { object: "core.turn_diagnostics"; session_id: string; turn_id: string; status: string; failure: DiagnosticFailure | null; items: ItemDiagnosticTiming[]; items_truncated: boolean };

const turnCodes = new Set(["authentication_error", "rate_limit_exceeded", "usage_limit_exceeded", "server_overloaded", "server_error", "invalid_request", "resource_not_found", "request_timeout", "context_length_exceeded", "cyber_policy", "connection_failed", "provider_billing_unavailable", "harness_error", "model_provider_required", "runtime_unavailable", "runtime_disconnected", "runtime_preparation_failed", "execution_interrupted", "delivery_unconfirmed", "input_rejected", "executor_protocol_error", "core_storage_failed", "internal_error"]);
const inputCodes = new Set(["environment_connection_timeout", "environment_unavailable", "model_provider_required", "internal_error"]);
const turnStatuses = new Set(["queued", "in_progress", "waiting", "completed", "failed", "cancelled"]);
const fields = (...values: string[]) => new Set(values);
function timestamp(value: unknown): value is string {
  return typeof value === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/u.test(value) && Number.isFinite(Date.parse(value));
}
function failure(value: unknown, codes: Set<string>, session = false): DiagnosticFailure {
  if (!isRecord(value) || !exactFields(value, fields("code", "params", "failed_at", ...(session ? ["source", ...(value.source === "turn" ? ["turn_id"] : [])] : []))) ||
      typeof value.code !== "string" || !codes.has(value.code) || !isRecord(value.params) || (value.failed_at !== null && !timestamp(value.failed_at))) return invalidAdminResponse();
  let params: DiagnosticFailure["params"] = {};
  if (value.code === "environment_provisioning_failed") {
    const p = value.params;
    if (!exactFields(p, fields("step", "index", "exit_code")) || (p.step !== null && (typeof p.step !== "string" || !["setup", "python", "npm", "system", "file", "skill"].includes(p.step))) ||
        (p.index !== null && (!Number.isSafeInteger(p.index) || (p.index as number) < 0 || p.step !== "setup")) ||
        (p.exit_code !== null && (!Number.isSafeInteger(p.exit_code) || (p.exit_code as number) < 1 || (p.exit_code as number) > 255 || !["setup", "python", "npm", "system"].includes(p.step ?? "")))) return invalidAdminResponse();
    params = { step: p.step as ProvisioningFailureParams["step"], index: p.index as number | null, exit_code: p.exit_code as number | null };
  } else if (value.code === "connection_failed" || value.code === "provider_billing_unavailable") {
    const p = value.params;
    if (!exactFields(p, fields("http_status")) || (p.http_status !== null && (!Number.isSafeInteger(p.http_status) || (p.http_status as number) < 100 || (p.http_status as number) > 599))) return invalidAdminResponse();
    if (value.code === "provider_billing_unavailable" && p.http_status !== null && p.http_status !== 402) return invalidAdminResponse();
    params = { http_status: p.http_status as number | null };
  } else if (Object.keys(value.params).length !== 0) return invalidAdminResponse();
  return { code: value.code as DiagnosticFailureCode, params, failed_at: value.failed_at as string | null };
}

export function projectSessionDiagnostics(value: unknown, sessionId: string): SessionDiagnostics {
  if (!isRecord(value) || !exactFields(value, fields("object", "session_id", "status", "failure")) || value.object !== "core.session_diagnostics" ||
      canonicalUuid(value.session_id) === null || !sameResourceId(value.session_id as string, sessionId) || typeof value.status !== "string" || !["idle", "in_progress", "requires_action", "failed"].includes(value.status)) return invalidAdminResponse();
  let projected: SessionDiagnosticFailure | null = null;
  if (value.status === "failed") {
    if (!isRecord(value.failure) || typeof value.failure.source !== "string" || !["turn", "environment", "environment_input"].includes(value.failure.source)) return invalidAdminResponse();
    const f = value.failure;
    const source = f.source as SessionDiagnosticFailure["source"];
    projected = { ...failure(f, source === "turn" ? turnCodes : source === "environment_input" ? inputCodes : new Set(["environment_provisioning_failed"]), true), source };
    if (source === "turn") {
      if (canonicalUuid(f.turn_id) === null) return invalidAdminResponse();
      projected.turn_id = f.turn_id as string;
    }
  } else if (value.failure !== null) return invalidAdminResponse();
  return { object: "core.session_diagnostics", session_id: value.session_id as string, status: value.status as SessionDiagnostics["status"], failure: projected };
}

export function projectTurnDiagnostics(value: unknown, sessionId: string, turnId: string): TurnDiagnostics {
  if (!isRecord(value) || !exactFields(value, fields("object", "session_id", "turn_id", "status", "failure", "items", "items_truncated")) || value.object !== "core.turn_diagnostics" ||
      canonicalUuid(value.session_id) === null || !sameResourceId(value.session_id as string, sessionId) || canonicalUuid(value.turn_id) === null || !sameResourceId(value.turn_id as string, turnId) ||
      typeof value.status !== "string" || !turnStatuses.has(value.status) || !Array.isArray(value.items) || value.items.length > 1000 || typeof value.items_truncated !== "boolean" || (value.items_truncated && value.items.length !== 1000)) return invalidAdminResponse();
  const projected = value.status === "failed" ? failure(value.failure, turnCodes) : null;
  if (value.status !== "failed" && value.failure !== null) return invalidAdminResponse();
  const seen = new Set<string>();
  const items: ItemDiagnosticTiming[] = value.items.map((item: unknown) => {
    if (!isRecord(item) || !exactFields(item, fields("item_id", "started_at", "completed_at", "observed_duration_ms")) || canonicalUuid(item.item_id) === null || !timestamp(item.started_at) ||
        (item.completed_at !== null && !timestamp(item.completed_at)) || (item.completed_at === null ? item.observed_duration_ms !== null : !Number.isSafeInteger(item.observed_duration_ms))) return invalidAdminResponse();
    const id = canonicalUuid(item.item_id)!;
    if (seen.has(id)) return invalidAdminResponse();
    seen.add(id);
    return { item_id: item.item_id as string, started_at: item.started_at, completed_at: item.completed_at as string | null, observed_duration_ms: item.observed_duration_ms as number | null };
  });
  return { object: "core.turn_diagnostics", session_id: value.session_id as string, turn_id: value.turn_id as string, status: value.status as string, failure: projected, items, items_truncated: value.items_truncated };
}
