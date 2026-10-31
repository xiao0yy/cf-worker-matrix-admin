import {createRemoteJWKSet, jwtVerify, errors} from 'jose';

// Access headers are untrusted until their signatures and application audience
// have been verified. Never derive a key URL from a token or request header.
let cachedIssuer, cachedKeys;
export function accessConfig(env) {
  const issuer = (env.TEAM_DOMAIN || '').replace(/\/$/, '');
  if (!/^https:\/\/[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.cloudflareaccess\.com$/.test(issuer)
      || !/^[a-f0-9]{64}$/i.test(env.POLICY_AUD || '')) return null;
  return {issuer, audience: env.POLICY_AUD};
}

export async function verifyAccess(request, env, resolver) {
  const config = accessConfig(env);
  if (!config) return {status: 503, error: '请配置 Access 的 TEAM_DOMAIN 和 POLICY_AUD'};
  const token = request.headers.get('Cf-Access-Jwt-Assertion');
  if (!token || token.length > 16384) return {status: 401, error: '请通过 Cloudflare Access 登录后重新打开管理页面'};
  if (!resolver) {
    if (cachedIssuer !== config.issuer) {
      cachedIssuer = config.issuer;
      cachedKeys = createRemoteJWKSet(new URL(config.issuer + '/cdn-cgi/access/certs'),
        {timeoutDuration: 5000, cooldownDuration: 30000, cacheMaxAge: 600000});
    }
    resolver = cachedKeys;
  }
  try {
    const {payload} = await jwtVerify(token, resolver, {
      ...config, algorithms: ['RS256'], requiredClaims: ['exp', 'iat', 'sub', 'email', 'type'],
    });
    if (payload.type !== 'app' || typeof payload.email !== 'string' || !payload.email.trim()
        || typeof payload.sub !== 'string' || !payload.sub) throw new Error('Not a user application token');
    return {status: 200};
  } catch (error) {
    if (error instanceof errors.JWKSTimeout || !(error instanceof errors.JOSEError) && error.message !== 'Not a user application token') {
      return {status: 503, error: 'Access 公钥验证暂时不可用，请稍后重试'};
    }
    return {status: 401, error: 'Access 登录已失效或不属于此应用，请重新登录'};
  }
}
