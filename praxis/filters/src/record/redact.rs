//! Port of the credential removal in `core/redact/headers.go`.

use http::HeaderMap;

/// The words of a name that make a header or a query parameter a credential.
const CREDENTIAL_WORDS: [&str; 11] = [
    "key",
    "apikey",
    "token",
    "auth",
    "authorization",
    "cookie",
    "secret",
    "signature",
    "password",
    "credential",
    "credentials",
];

/// Reports whether `name`, split at `-` and `_` with case ignored, has one of the credential words.
pub fn credential_name(name: &str) -> bool {
    name.to_lowercase()
        .split(['-', '_'])
        .any(|w| CREDENTIAL_WORDS.contains(&w))
}

/// A copy of `h` without credential headers, for storing.
pub fn headers(h: &HeaderMap) -> HeaderMap {
    let mut out = HeaderMap::with_capacity(h.len());
    for (k, v) in h {
        if !credential_name(k.as_str()) {
            out.append(k, v.clone());
        }
    }
    out
}

/// `raw` without credential parameters, for storing. The rest keeps its order and escaping,
/// since a replay must send the same bytes. A query that Go's `url.ParseQuery` refuses (a `;`
/// or a bad `%` escape) is dropped whole: what it holds cannot be told apart.
pub fn query(raw: &str) -> String {
    let parses = raw.split('&').all(|p| {
        let (name, value) = p.split_once('=').unwrap_or((p, ""));
        !p.contains(';') && unescape(name).is_some() && unescape(value).is_some()
    });
    if !parses {
        return String::new();
    }
    raw.split('&')
        .filter(|p| {
            let name = p.split_once('=').map_or(*p, |(n, _)| n);
            !unescape(name).is_some_and(|n| credential_name(&n))
        })
        .collect::<Vec<_>>()
        .join("&")
}

/// Go's `url.QueryUnescape`: `+` is a space, `%` takes two hex digits, `None` on a bad escape.
fn unescape(s: &str) -> Option<String> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'%' => {
                let hex = b.get(i + 1..i + 3)?;
                if !hex.iter().all(u8::is_ascii_hexdigit) {
                    return None;
                }
                out.push(u8::from_str_radix(std::str::from_utf8(hex).ok()?, 16).ok()?);
                i += 3;
            }
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            c => {
                out.push(c);
                i += 1;
            }
        }
    }
    Some(String::from_utf8_lossy(&out).into_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use http::HeaderValue;

    /// The keys of Go's `SecretHeaders`.
    const SECRET_HEADERS: [&str; 7] = [
        "authorization",
        "proxy-authorization",
        "cookie",
        "set-cookie",
        "x-api-key",
        "api-key",
        "x-goog-api-key",
    ];

    fn one(name: &'static str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert(name, HeaderValue::from_static("s"));
        h
    }

    #[test]
    fn headers_drops_credentials_and_copies() {
        let mut input = HeaderMap::new();
        input.insert("Authorization", HeaderValue::from_static("Bearer x"));
        input.insert("X-Api-Key", HeaderValue::from_static("k"));
        input.insert("cookie", HeaderValue::from_static("c"));
        input.append("Anthropic-Beta", HeaderValue::from_static("a"));
        input.append("Anthropic-Beta", HeaderValue::from_static("b"));
        let mut out = headers(&input);
        assert_eq!(
            out.keys_len(),
            1,
            "headers = {out:?}, want only Anthropic-Beta"
        );
        assert_eq!(out.get_all("Anthropic-Beta").iter().count(), 2);
        if let Some(v) = out.get_mut("Anthropic-Beta") {
            *v = HeaderValue::from_static("changed");
        }
        assert_eq!(
            input.get("Anthropic-Beta").unwrap(),
            "a",
            "headers shares its values with its input"
        );
    }

    // Some APIs take the key in the URL (Gemini's ?key=). A captured query keeps everything else.
    #[test]
    fn query_drops_credential_parameters() {
        for (input, want) in [
            ("beta=true", "beta=true"),
            ("key=AIza123&alt=sse", "alt=sse"),
            ("api_key=k&apikey=k&access_token=t", ""),
            ("token=t&x=1", "x=1"),
            ("z=1&a=%2F&b", "z=1&a=%2F&b"),
            ("z=1&key=k&a=2", "z=1&a=2"),
            ("%zz", ""),
            ("", ""),
        ] {
            assert_eq!(query(input), want, "query({input:?})");
        }
    }

    // Not a Go case table: the refusals of Go's url.ParseQuery that `query` reproduces.
    #[test]
    fn query_drops_what_go_refuses() {
        for (input, want) in [
            ("a=1;b=2", ""),
            ("a=%2", ""),
            ("a=%+1", ""),
            ("x=1&a=%zz", ""),
            ("x%5Fkey=1&a+key=2&x=%41", "a+key=2&x=%41"),
            ("&&x=1", "&&x=1"),
        ] {
            assert_eq!(query(input), want, "query({input:?})");
        }
    }

    #[test]
    fn headers_drops_cloud_and_proxy_credentials() {
        for h in [
            "X-Amz-Security-Token",
            "X-Auth-Token",
            "X-Authorization",
            "X-Session-Token",
            "X-Secret-Token",
            "X-Csrf-Token",
            "X-Platform-Secret",
            "X-Authbridge-Secret",
        ] {
            assert!(headers(&one(h)).is_empty(), "{h} survived");
        }
    }

    // A credential is judged by the words of its name, split at "-" and "_", so a header or
    // parameter with a credential word needs no entry of its own.
    #[test]
    fn headers_and_query_drop_names_with_a_credential_word() {
        let names = [
            "Ocp-Apim-Subscription-Key",
            "Private-Token",
            "X-Vault-Token",
            "X-Goog-Iam-Authorization-Token",
        ];
        for h in names.into_iter().chain(SECRET_HEADERS) {
            assert!(headers(&one(h)).is_empty(), "{h} survived");
        }
        let input = "X-Amz-Credential=c&X-Amz-Signature=s&X-Amz-Security-Token=t&subscription-key=k&X-Amz-Expires=60";
        assert_eq!(query(input), "X-Amz-Expires=60", "query({input:?})");
        assert_eq!(
            query("max_tokens=5&pageToken=p"),
            "max_tokens=5&pageToken=p",
            "neither name has a credential word"
        );
    }

    // The names docs/laptop-service.md gives as examples of the word rule.
    #[test]
    fn credential_name_documented_examples() {
        for (name, want) in [
            ("Idempotency-Key", true),
            ("Sec-WebSocket-Key", true),
            ("accessToken", false),
        ] {
            assert_eq!(credential_name(name), want, "credential_name({name:?})");
        }
    }
}
