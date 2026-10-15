import { expect, it } from "vitest";
import { AdminClient } from "./admin-client";
import { projectSessionDiagnostics, projectTurnDiagnostics, upstreamFingerprintTuples, type UpstreamRejectionParams } from "./session-diagnostics";
import fingerprintFixture from "./fixtures/upstream-fingerprints.json";

const id = "a113b7db-c689-4dfc-af9a-04bd0e466f11";
const turn = "729dab72-a9ea-4d0a-a38c-1b45e46233a9";
const item = "bf8d4f3a-d8bd-4d59-ad37-15f186f90832";
const time = "2026-09-28T03:00:00.123456Z";
const failure = { code: "runtime_disconnected", params: {}, failed_at: time };
const session = { object: "core.session_diagnostics", session_id: id, status: "failed", failure: { ...failure, source: "turn", turn_id: turn } };
const snapshot = { object: "core.turn_diagnostics", session_id: id, turn_id: turn, status: "failed", failure, items: [{ item_id: item, started_at: time, completed_at: null, observed_duration_ms: null }], items_truncated: false };

it("retrieves Core diagnostics with credential, scope and cancellation", async () => {
  const abort = new AbortController();
  const paths: string[] = [];
  const client = new AdminClient({ adminToken: "admin", fetch: (async (url, init) => {
    paths.push(String(url));
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer admin");
    expect(init?.signal).toBe(abort.signal);
    return new Response(JSON.stringify(String(url).includes("/turns/") ? snapshot : session), { headers: { "Content-Type": "application/json" } });
  }) as typeof fetch });
  expect(await client.retrieveSessionDiagnostics("project", id, { signal: abort.signal })).toEqual(session);
  expect(await client.retrieveTurnDiagnostics("project", id, turn, { signal: abort.signal })).toEqual(snapshot);
  expect(paths).toEqual([`/core/v1/projects/project/sessions/${id}/diagnostics`, `/core/v1/projects/project/sessions/${id}/turns/${turn}/diagnostics`]);
});

it("preserves historical nulls and finite safe provisioning parameters", () => {
  const value = { ...session, failure: { source: "environment", code: "environment_provisioning_failed", params: { step: "setup", index: 2, exit_code: 7 }, failed_at: null } };
  expect(projectSessionDiagnostics(value, id)).toEqual(value);
  expect(projectTurnDiagnostics(snapshot, id.toUpperCase(), turn.toUpperCase())).toEqual(snapshot);
});

it.each([
  (v: any) => { v.failure.error = "secret-canary"; },
  (v: any) => { v.failure.code = "input_secret-canary"; },
  (v: any) => { v.failure.params.message = "secret-canary"; },
  (v: any) => { v.session_id = turn; },
  (v: any) => { v.turn_id = id; },
  (v: any) => { v.items[0].observed_duration_ms = 0; },
  (v: any) => { v.items[0].completed_at = "bad"; },
  (v: any) => { v.items.push(v.items[0]); },
  (v: any) => { v.items_truncated = true; },
  (v: any) => { v.items = Array.from({ length: 1001 }, () => v.items[0]); },
  (v: any) => { v.status = "completed"; },
])("rejects malformed, unsafe or mis-scoped Turn snapshots", (mutate) => {
  const value = structuredClone(snapshot); mutate(value);
  expect(() => projectTurnDiagnostics(value, id, turn)).toThrow();
  expect(() => projectTurnDiagnostics(value, id, turn)).not.toThrow(/secret-canary/u);
});

it.each([
  { step: "secret-canary", index: null, exit_code: null },
  { step: "python", index: 3, exit_code: 2 },
  { step: "setup", index: Number.MAX_SAFE_INTEGER + 1, exit_code: 1 },
  { step: "file", index: null, exit_code: 1 },
  { step: "setup", index: 0, exit_code: 256 },
])("rejects unsafe provisioning parameters", (params) => {
  const value = { ...session, failure: { source: "environment", code: "environment_provisioning_failed", params, failed_at: time } };
  expect(() => projectSessionDiagnostics(value, id)).toThrow();
});

it.each(["authentication_error", "rate_limit_exceeded", "usage_limit_exceeded", "server_overloaded", "server_error", "invalid_request", "resource_not_found", "request_timeout", "context_length_exceeded", "cyber_policy", "connection_failed"])("projects finite native category %s", (code) => {
  const params = code === "connection_failed" ? { http_status: 503 } : {};
  const failed = { ...failure, code, params };
  expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
  expect(projectSessionDiagnostics({ ...session, failure: { ...failed, source: "turn", turn_id: turn } }, id).failure?.code).toBe(code);
  expect(() => projectSessionDiagnostics({ ...session, failure: { ...failed, source: "environment_input" } }, id)).toThrow();
});
const upstreamCodes = ["model_input_unsupported_image", "model_provider_format_rejected", "model_reasoning_state_invalid", "model_input_ends_with_assistant"];
const upstreamParams = { fingerprint: "model_no_image_input", summary: "selected model does not accept image input", upstream_request_id: "88a8d808-59c1-4bf3-8950-b34ce57e028b" };
// Mirrors the Go test TestUpstream400CodesAreInEveryEnumAndMatchTheProtoMapping: the table must produce exactly these four codes.
it("keeps the fingerprint table's code set equal to the four contract literals", () => {
  expect([...new Set(Object.values(upstreamFingerprintTuples).map((tuple) => tuple.code))].sort()).toEqual([...upstreamCodes].sort());
});
it.each(upstreamCodes)("projects upstream 400 category %s with and without params", (code) => {
  const tuple = { model_input_unsupported_image: upstreamParams, model_provider_format_rejected: { ...upstreamParams, fingerprint: "provider_messages_required", summary: "upstream provider rejected the Responses request body (messages field required)" },
    model_reasoning_state_invalid: { ...upstreamParams, fingerprint: "reasoning_encrypted_unverifiable", summary: "upstream could not verify encrypted reasoning content carried in thread history" },
    model_input_ends_with_assistant: { ...upstreamParams, fingerprint: "trailing_model_turn", summary: "upstream rejected a request that ends with a model turn" } }[code]!;
  for (const params of [{}, { fingerprint: tuple.fingerprint, summary: tuple.summary }, tuple]) {
    const failed = { ...failure, code, params };
    expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
    expect(projectSessionDiagnostics({ ...session, failure: { ...failed, source: "turn", turn_id: turn } }, id).failure?.code).toBe(code);
  }
  expect(() => projectSessionDiagnostics({ ...session, failure: { ...failure, code, params: {}, source: "environment_input" } }, id)).toThrow();
});
it.each([
  { fingerprint: "other_print", summary: "x" },
  { fingerprint: "model_no_image_input" },
  { summary: "x" },
  { fingerprint: "model_no_image_input", summary: "" },
  { fingerprint: "model_no_image_input", summary: "x".repeat(241) },
  { fingerprint: "model_no_image_input", summary: "x", upstream_request_id: "Bearer secret-canary" },
  { fingerprint: "model_no_image_input", summary: "x", message: "secret-canary" },
  { fingerprint: ["model_no_image_input"], summary: "x" },
  { http_status: 400 },
])("rejects unsafe upstream 400 parameters", (params) => {
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params } }, id, turn)).toThrow();
});
const recoveryParams = { recovery_count: 1, recovery_fingerprints: ["reasoning_encrypted_unverifiable"], recovery_outcomes: ["exhausted"], recovery_dropped_reasoning_items: 4, recovery_dropped_images: 0 };
it("projects recovery facts of a failed upstream 400 Turn", () => {
  const code = "model_input_unsupported_image";
  for (const params of [{ ...upstreamParams, ...recoveryParams }, { fingerprint: upstreamParams.fingerprint, summary: upstreamParams.summary, ...recoveryParams, recovery_count: 0, recovery_outcomes: ["aborted"] },
    { ...upstreamParams, ...recoveryParams, recovery_count: 1, recovery_fingerprints: ["model_no_image_input", "trailing_model_turn"], recovery_outcomes: ["exhausted", "aborted"] },
    { ...upstreamParams, ...recoveryParams, recovery_count: 2 }]) {
    const failed = { ...failure, code, params };
    expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
  }
});
it.each([
  { recovery_count: 3 },
  { recovery_count: -1 },
  { recovery_count: 1.5 },
  { recovery_count: "1" },
  { recovery_fingerprints: [] },
  { recovery_fingerprints: ["model_no_image_input", "model_no_image_input", "model_no_image_input"], recovery_outcomes: ["aborted", "aborted", "aborted"] },
  { recovery_fingerprints: ["secret-canary"] },
  { recovery_fingerprints: ["provider_messages_required"] },
  { recovery_fingerprints: "reasoning_encrypted_unverifiable" },
  { recovery_outcomes: ["recovered"] },
  { recovery_outcomes: ["exhausted", "aborted"] },
  { recovery_count: 1, recovery_outcomes: ["aborted"] },
  { recovery_count: 2, recovery_fingerprints: ["model_no_image_input", "trailing_model_turn"], recovery_outcomes: ["aborted", "exhausted"] },
  { recovery_count: 0, recovery_fingerprints: ["model_no_image_input", "trailing_model_turn"], recovery_outcomes: ["aborted", "aborted"] },
  { recovery_count: 0, recovery_outcomes: ["exhausted"] },
  { recovery_count: 1, recovery_outcomes: ["exhausted", "exhausted"], recovery_fingerprints: ["model_no_image_input", "trailing_model_turn"] },
  { recovery_dropped_reasoning_items: 2_000_001 },
  { recovery_dropped_images: 2_000_001 },
  { recovery_dropped_images: -1 },
  { recovery_dropped_images: null },
])("rejects unsafe recovery facts %j", (override) => {
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params: { ...upstreamParams, ...recoveryParams, ...override } } }, id, turn)).toThrow();
});
it("rejects partial or extra recovery keys and recovery facts on other categories", () => {
  const { recovery_dropped_images: _omitted, ...partial } = recoveryParams;
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params: { ...upstreamParams, ...partial } } }, id, turn)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params: { ...upstreamParams, ...recoveryParams, recovery_native_session_id: "secret-canary" } } }, id, turn)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_reasoning_state_invalid", params: { ...recoveryParams } } }, id, turn)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "server_error", params: { ...upstreamParams, ...recoveryParams } } }, id, turn)).toThrow();
});
it.each(["authentication_error", "server_error", "rate_limit_exceeded"])("projects recovery facts when the final failure is %s", (code) => {
  const failed = { ...failure, code, params: { ...recoveryParams } };
  expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failed, params: { ...recoveryParams, recovery_count: 3 } } }, id, turn)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failed, params: { ...recoveryParams, extra: "x" } } }, id, turn)).toThrow();
});
it("projects recovery facts when the final failure is connection_failed", () => {
  const failed = { ...failure, code: "connection_failed", params: { http_status: 503, ...recoveryParams } };
  expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
});
it("rejects a fingerprint or summary that does not match the failure code", () => {
  for (const params of [{ ...upstreamParams, fingerprint: "trailing_model_turn" }, { ...upstreamParams, summary: "something else" }]) {
    expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params } }, id, turn)).toThrow();
  }
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_ends_with_assistant", params: upstreamParams } }, id, turn)).toThrow();
});
it("rejects upstream 400 parameters on other categories", () => {
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "server_error", params: upstreamParams } }, id, turn)).toThrow();
});
it.each([null, 100, 429, 599])("preserves safe connection status %s", (http_status) => {
  const failed = { ...failure, code: "connection_failed", params: { http_status } };
  expect(projectTurnDiagnostics({ ...snapshot, failure: failed }, id, turn).failure).toEqual(failed);
});
it.each([{}, { http_status: 99 }, { http_status: 600 }, { http_status: 503.5 }, { http_status: "503" }, { http_status: 503, message: "secret-canary" }])("rejects unsafe connection parameters", (params) => {
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "connection_failed", params } }, id, turn)).toThrow();
});
it("rejects HTTP status parameters on nonconnection categories", () => {
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "authentication_error", params: { http_status: 401 } } }, id, turn)).toThrow();
});

const provisioning = { source: "environment", code: "environment_provisioning_failed", params: { step: "setup", index: null, exit_code: null }, failed_at: null };
it.each([
  ["Session status", () => projectSessionDiagnostics({ ...session, status: ["failed"], failure: null }, id)],
  ["Turn status", () => projectTurnDiagnostics({ ...snapshot, status: ["failed"], failure: null }, id, turn)],
  ["failure source", () => projectSessionDiagnostics({ ...session, failure: { ...provisioning, source: ["environment"] } }, id)],
  ["provisioning step", () => projectSessionDiagnostics({ ...session, failure: { ...provisioning, params: { ...provisioning.params, step: ["setup"] } } }, id)],
] as const)("rejects arrays in %s", (_field, project) => {
  expect(project).toThrow();
});

 it.each([402, null])("retrieves billing diagnostics for Session and Turn with status %s", async (http_status) => {
  const f = { ...failure, code: "provider_billing_unavailable", params: { http_status } };
  const sessionValue = { ...session, failure: { ...f, source: "turn", turn_id: turn } };
  const turnValue = { ...snapshot, failure: f };
  const client = new AdminClient({ adminToken: "admin", fetch: (async (url) => new Response(JSON.stringify(String(url).includes("/turns/") ? turnValue : sessionValue), { headers: { "Content-Type": "application/json" } })) as typeof fetch });
  expect(await client.retrieveSessionDiagnostics("project", id)).toEqual(sessionValue);
  expect(await client.retrieveTurnDiagnostics("project", id, turn)).toEqual(turnValue);
  expect(() => projectSessionDiagnostics({ ...session, failure: { ...f, source: "environment_input" } }, id)).toThrow();
});
it.each([{}, { http_status: 400 }, { http_status: 403 }, { http_status: 500 }, { http_status: 401 }, { http_status: 429 }, { http_status: "402" }, { http_status: 402.5 }, { http_status: 402, message: "private-canary" }])("rejects malformed billing parameters on both routes", (params) => {
  const f = { ...failure, code: "provider_billing_unavailable", params };
  expect(() => projectSessionDiagnostics({ ...session, failure: { ...f, source: "turn", turn_id: turn } }, id)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: f }, id, turn)).toThrow();
});
it("accepts the two-record aggregate of dropped recovery items", () => {
  const params = { ...upstreamParams, ...recoveryParams, recovery_count: 2, recovery_fingerprints: ["reasoning_encrypted_unverifiable", "reasoning_encrypted_unverifiable"], recovery_outcomes: ["exhausted", "exhausted"], recovery_dropped_reasoning_items: 2_000_000, recovery_dropped_images: 2_000_000 };
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: { ...failure, code: "model_input_unsupported_image", params } }, id, turn)).not.toThrow();
});

it("UpstreamRejectionParams rejects a partial recovery group at compile time", () => {
  // @ts-expect-error recovery fields are all-or-nothing
  const partial: UpstreamRejectionParams = { fingerprint: "model_no_image_input", summary: "selected model does not accept image input", recovery_count: 1 };
  expect(partial).toBeDefined();
});
it("keeps the fingerprint table identical to the fixture Core is tested against", () => {
  expect(upstreamFingerprintTuples).toEqual(fingerprintFixture);
});
