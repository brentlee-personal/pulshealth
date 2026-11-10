import { oauthKeyFixture } from "./keyFixture";
import { generateKeyPairSync, verify } from "node:crypto";
import { describe, expect, it } from "vitest";

import { ACCESS_TOKEN_SECONDS, OAUTH_SCOPE } from "./config";
import { issueAccessToken, signAccessToken, verifyAccessToken } from "./jwt";

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
