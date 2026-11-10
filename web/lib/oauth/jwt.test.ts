import { oauthKeyFixture } from "./keyFixture";
import { generateKeyPairSync, verify } from "node:crypto";
import { describe, expect, it } from "vitest";

import { ACCESS_TOKEN_SECONDS, OAUTH_SCOPE } from "./config";
import { issueAccessToken, signAccessToken, verifyAccessToken } from "./jwt";
import { CATALOG } from "../catalog";

// The shared test vector (the MCP server's verifier tests against the same one).
const oauthKeys = oauthKeyFixture();
const VECTOR =
  "eyJhbGciOiJFZERTQSIsInR5cCI6ImF0K2p3dCIsImtpZCI6InJmYzgwMzItdGVzdC0xIn0.eyJpc3MiOiJodHRwczovL3ZpZXdlci5leGFtcGxlLmNvbSIsImF1ZCI6Imh0dHBzOi8vbWNwLmV4YW1wbGUuY29tL21jcCIsInN1YiI6IjVlYTRkMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMSIsImNsaWVudF9pZCI6InBjX3Rlc3QiLCJzY29wZSI6ImhlYWx0aDpyZWFkIiwiaWF0IjoxNzkwMDAwMDAwLCJleHAiOjE3OTAwMDE4MDAsImp0aSI6IkFBQUFBQUFBQUFBQUFBQUFBQUFBQUEifQ.jkcgRYALjcIxpK9zgBjCn2BzUTqYCHFkiCsdKuBmZbhizq6JWvgUIja1pFnb_Ci4W09ZFtT-ZXUv5NsblWRJCg";
const CLAIMS = {
  iss: "https://viewer.example.com",
  aud: "https://mcp.example.com/mcp",
  sub: "5ea4d000-0000-4000-8000-000000000001",
  client_id: "pc_test",
  scope: "health:read",
  iat: 1790000000,
  exp: 1790001800,
  jti: "AAAAAAAAAAAAAAAAAAAAAA",
};

const decode = (part: string) => JSON.parse(Buffer.from(part, "base64url").toString("utf8"));

describe("signAccessToken", () => {
  it("signs the fixed personal API assertion used by the Go verifier", () => {
    const claims = {
      "iss": "https://viewer.example.com",
      "aud": "family-health-api",
      "sub": "5ea4d000-0000-4000-8000-000000000001",
      "client_id": "pc_test",
      "iat": 1790000000,
      "exp": 1790000060,
      "jti": "5ea4d000-0000-4000-8000-000000000002",
      "grant_id": "5ea4d000-0000-4000-8000-000000000003",
      "principal": {
        "member_id": "5ea4d000-0000-4000-8000-000000000001",
        "credential_id": "5ea4d000-0000-4000-8000-000000000002",
        "allowed_types": [
          "HKQuantityTypeIdentifierStepCount"
        ],
        "allowed_time_range": {
          "from": "2026-01-01T00:00:00.000000001Z",
          "until": "2026-02-01T00:00:00Z"
        },
        "authorization_version": 1,
        "credential_kind": "exchange",
        "allowed_operations": [
          "metrics:read"
        ]
      }
    } satisfies import("./jwt").APIAssertionClaims;
    expect(signAccessToken(claims, oauthKeys.signingKey)).toBe("eyJhbGciOiJFZERTQSIsInR5cCI6ImF0K2p3dCIsImtpZCI6InJmYzgwMzItdGVzdC0xIn0.eyJpc3MiOiJodHRwczovL3ZpZXdlci5leGFtcGxlLmNvbSIsImF1ZCI6ImZhbWlseS1oZWFsdGgtYXBpIiwic3ViIjoiNWVhNGQwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxIiwiY2xpZW50X2lkIjoicGNfdGVzdCIsImlhdCI6MTc5MDAwMDAwMCwiZXhwIjoxNzkwMDAwMDYwLCJqdGkiOiI1ZWE0ZDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDIiLCJncmFudF9pZCI6IjVlYTRkMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMyIsInByaW5jaXBhbCI6eyJtZW1iZXJfaWQiOiI1ZWE0ZDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDEiLCJjcmVkZW50aWFsX2lkIjoiNWVhNGQwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAyIiwiYWxsb3dlZF90eXBlcyI6WyJIS1F1YW50aXR5VHlwZUlkZW50aWZpZXJTdGVwQ291bnQiXSwiYWxsb3dlZF90aW1lX3JhbmdlIjp7ImZyb20iOiIyMDI2LTAxLTAxVDAwOjAwOjAwLjAwMDAwMDAwMVoiLCJ1bnRpbCI6IjIwMjYtMDItMDFUMDA6MDA6MDBaIn0sImF1dGhvcml6YXRpb25fdmVyc2lvbiI6MSwiY3JlZGVudGlhbF9raW5kIjoiZXhjaGFuZ2UiLCJhbGxvd2VkX29wZXJhdGlvbnMiOlsibWV0cmljczpyZWFkIl19fQ.sWyBGrw60ZzqW8YGvB4oI7ZCI2jGdXl7eg1ZMRXvlaIcVeMjEPuX7d1EIRMJpnT7TyTsCwSixGM6MSzf1YQ2DQ");
  });

  it("reproduces the contract's test vector byte for byte", () => {
    expect(signAccessToken(CLAIMS, oauthKeys.signingKey)).toBe(VECTOR);
  });

  it("serialises the claims in the contract's order whatever order they come in", () => {
    const shuffled = {
      jti: CLAIMS.jti,
      exp: CLAIMS.exp,
      scope: CLAIMS.scope,
      sub: CLAIMS.sub,
      iat: CLAIMS.iat,
      client_id: CLAIMS.client_id,
      aud: CLAIMS.aud,
      iss: CLAIMS.iss,
    };
    expect(signAccessToken(shuffled, oauthKeys.signingKey)).toBe(VECTOR);
  });

  it("uses the fixed Ed25519 header and verifies with the public key alone", () => {
    const [h, p, s] = VECTOR.split(".");
    expect(Buffer.from(h, "base64url").toString()).toBe('{"alg":"EdDSA","typ":"at+jwt","kid":"rfc8032-test-1"}');
    expect(VECTOR).not.toContain("=");
    expect(verify(null, Buffer.from(`${h}.${p}`), oauthKeys.publicKey, Buffer.from(s, "base64url"))).toBe(true);
  });

  it("changes the signature with the private key", () => {
    expect(signAccessToken(CLAIMS, { id: oauthKeys.signingKey.id, privateKey: generateKeyPairSync("ed25519").privateKey }).split(".")[2]).not.toBe(VECTOR.split(".")[2]);
  });
});

describe("issueAccessToken", () => {
  it("binds all selected catalog types and the authorization version in the signature", () => {
    const config = { issuer: CLAIMS.iss, resource: CLAIMS.aud, ...oauthKeys };
    const permissions = { operations: ["metrics:read"], record_types: CATALOG.map((type) => type.identifier),
      time_range: { from: "2020-01-01T00:00:00Z", until: "2030-01-01T00:00:00Z" } } satisfies import("./permissions").ReadPermissions;
    const grantId = "5ea4d000-0000-4000-8000-000000000003";
    const issued = issueAccessToken(config, CLAIMS.sub, CLAIMS.client_id, CLAIMS.iat, { grantId, permissions, authorizationVersion: 7 });
    const verified = verifyAccessToken(issued.token, config, CLAIMS.iat + 1);
    expect(verified?.grant_id).toBe(grantId);
    expect(verified?.principal).toEqual({ member_id: CLAIMS.sub, credential_id: grantId, credential_kind: "grant",
      allowed_operations: permissions.operations, allowed_types: permissions.record_types, allowed_time_range: permissions.time_range, authorization_version: 7 });
    expect(issued.token.length).toBeLessThanOrEqual(16_384);
    const parts = issued.token.split(".");
    const claims = decode(parts[1]);
    claims.principal.allowed_operations.push("samples:read");
    parts[1] = Buffer.from(JSON.stringify(claims)).toString("base64url");
    expect(verifyAccessToken(parts.join("."), config, CLAIMS.iat + 1)).toBeNull();
  });

  it("issues 30-minute tokens for the configured issuer and audience, with a fresh jti", () => {
    const config = { issuer: "https://viewer.example.com", resource: "https://mcp.example.com/mcp", ...oauthKeys };
    const a = issueAccessToken(config, "5EA4D000-0000-4000-8000-000000000001", "pc_abc", 1_800_000_000);
    const b = issueAccessToken(config, "5ea4d000-0000-4000-8000-000000000001", "pc_abc", 1_800_000_000);
    expect(a.expiresIn).toBe(ACCESS_TOKEN_SECONDS);
    expect(ACCESS_TOKEN_SECONDS).toBe(1800);
    const claims = decode(a.token.split(".")[1]);
    expect(Object.keys(claims)).toEqual(["iss", "aud", "sub", "client_id", "scope", "iat", "exp", "jti"]);
    expect(claims).toMatchObject({
      iss: config.issuer,
      aud: config.resource,
      sub: "5ea4d000-0000-4000-8000-000000000001",
      client_id: "pc_abc",
      scope: OAUTH_SCOPE,
      iat: 1_800_000_000,
      exp: 1_800_001_800,
    });
    expect(Buffer.from(claims.jti, "base64url")).toHaveLength(16);
    expect(decode(b.token.split(".")[1]).jti).not.toBe(claims.jti);
    // Re-signing the decoded claims preserves the deterministic signature.
    expect(signAccessToken(claims, oauthKeys.signingKey)).toBe(a.token);
  });
});

describe("Ed25519 verification boundary", () => {
  const config = { issuer: CLAIMS.iss, resource: CLAIMS.aud, publicKeys: oauthKeys.publicKeys };
  it("accepts only the known active key and fixed issuer and audience", () => {
    expect(verifyAccessToken(VECTOR, config, CLAIMS.iat + 1)?.sub).toBe(CLAIMS.sub);
    for (const rejected of [
      { ...config, issuer: "https://other.example" },
      { ...config, resource: "https://other.example/api" },
      { ...config, publicKeys: new Map() },
    ]) expect(verifyAccessToken(VECTOR, rejected, CLAIMS.iat + 1)).toBeNull();
    const unknown = signAccessToken(CLAIMS, { ...oauthKeys.signingKey, id: "unknown" });
    expect(verifyAccessToken(unknown, config, CLAIMS.iat + 1)).toBeNull();
  });
});
