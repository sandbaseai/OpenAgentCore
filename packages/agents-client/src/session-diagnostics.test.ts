import { expect, it } from "vitest";
import { AdminClient } from "./admin-client";
import { projectSessionDiagnostics, projectTurnDiagnostics } from "./session-diagnostics";

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
it.each([{}, { http_status: 401 }, { http_status: 429 }, { http_status: "402" }, { http_status: 402.5 }, { http_status: 402, message: "private-canary" }])("rejects malformed billing parameters on both routes", (params) => {
  const f = { ...failure, code: "provider_billing_unavailable", params };
  expect(() => projectSessionDiagnostics({ ...session, failure: { ...f, source: "turn", turn_id: turn } }, id)).toThrow();
  expect(() => projectTurnDiagnostics({ ...snapshot, failure: f }, id, turn)).toThrow();
});
