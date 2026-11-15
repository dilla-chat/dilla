//! A community Bot's command catalog (packages/client-core/src/bot/catalog.ts): read once on connect and replaced live
//! by op 61. It is not secret (names and argument shapes) and display only: the Bot authorises every operation itself.

use crate::gateway::frames::Value;
use crate::state::bot::{ArgShape, ArgType, BotCommandSpec, BotStatus, BotStatusState};

/// `CatalogError`: the catalog is refused whole.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("E_BOT_CATALOG: {0}")]
pub struct CatalogError(pub &'static str);

const MAX_COMMANDS: usize = 256;
const MAX_ARGS: usize = 16;
const MAX_PUBLICATION: usize = 256;

fn arr(v: &Value, n: Option<usize>) -> Result<&[Value], CatalogError> {
    let a = v.as_array().ok_or(CatalogError("shape"))?;
    if n.is_some_and(|n| a.len() != n) {
        return Err(CatalogError("shape"));
    }
    Ok(a)
}

fn uint(v: &Value) -> Result<u64, CatalogError> {
    v.as_uint().ok_or(CatalogError("shape"))
}

fn text(v: &Value) -> Result<&str, CatalogError> {
    v.as_text().ok_or(CatalogError("shape"))
}

fn bit(v: &Value) -> Result<bool, CatalogError> {
    match uint(v)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(CatalogError("flag")),
    }
}

fn read_arg(v: &Value) -> Result<ArgShape, CatalogError> {
    let [name, ty, required] = arr(v, Some(3))? else {
        unreachable!("arity checked")
    };
    let name = text(name)?;
    if name.is_empty() || name.len() > 32 {
        return Err(CatalogError("argument name"));
    }
    Ok(ArgShape {
        name: name.to_owned(),
        ty: ArgType::from_code(uint(ty)?).ok_or(CatalogError("argument type"))?,
        required: bit(required)?,
    })
}

fn read_command(v: &Value) -> Result<BotCommandSpec, CatalogError> {
    let [
        name,
        description,
        operation,
        shape,
        permission,
        needs,
        handler,
    ] = arr(v, Some(7))?
    else {
        unreachable!("arity checked")
    };
    let (name, description) = (text(name)?, text(description)?);
    if name.is_empty() || name.len() > 64 || description.len() > 512 {
        return Err(CatalogError("name or description"));
    }
    let args = arr(shape, None)?
        .iter()
        .map(read_arg)
        .collect::<Result<Vec<_>, _>>()?;
    let mut names: Vec<&str> = args.iter().map(|a| a.name.as_str()).collect();
    names.sort_unstable();
    names.dedup();
    if args.len() > MAX_ARGS || names.len() != args.len() {
        return Err(CatalogError("arguments"));
    }
    Ok(BotCommandSpec {
        name: name.to_owned(),
        description: description.to_owned(),
        operation: u32::try_from(uint(operation)?).map_err(|_| CatalogError("operation"))?,
        args,
        permission_bit: uint(permission)?,
        needs_bot_channel: bit(needs)?,
        handler_kind: u32::try_from(uint(handler)?).map_err(|_| CatalogError("handler"))?,
    })
}

/// `COMMAND_NAME`: lower-case words of letters, digits and hyphens.
fn command_name(n: &str) -> bool {
    n.len() <= 32
        && n.split(' ').all(|w| {
            let mut c = w.chars();
            c.next()
                .is_some_and(|f| f.is_ascii_lowercase() || f.is_ascii_digit())
                && c.all(|ch| ch.is_ascii_lowercase() || ch.is_ascii_digit() || ch == '-')
        })
}

/// `ARG_NAME`: one such word, with `_` too, at most 32 bytes.
fn arg_name(n: &str) -> bool {
    let mut c = n.chars();
    n.len() <= 32
        && c.next()
            .is_some_and(|f| f.is_ascii_lowercase() || f.is_ascii_digit())
        && c.all(|ch| ch.is_ascii_lowercase() || ch.is_ascii_digit() || ch == '-' || ch == '_')
}

/// `decodeBotCommands`: read whole or refused (at most 256 commands, unique names); a command whose names are outside
/// the picker's grammar is dropped.
pub fn decode_commands(values: &[Value]) -> Result<Vec<BotCommandSpec>, CatalogError> {
    if values.len() > MAX_COMMANDS {
        return Err(CatalogError("too many commands"));
    }
    let commands = values
        .iter()
        .map(read_command)
        .collect::<Result<Vec<_>, _>>()?;
    let mut names: Vec<&str> = commands.iter().map(|c| c.name.as_str()).collect();
    names.sort_unstable();
    names.dedup();
    if names.len() != commands.len() {
        return Err(CatalogError("duplicate name"));
    }
    Ok(commands
        .into_iter()
        .filter(|c| command_name(&c.name) && c.args.iter().all(|a| arg_name(&a.name)))
        .collect())
}

/// The instance's state codes 0..=4.
fn state_of(code: u64) -> Option<BotStatusState> {
    Some(match code {
        0 => BotStatusState::Ready,
        1 => BotStatusState::Degraded,
        2 => BotStatusState::Offline,
        3 => BotStatusState::Disabled,
        4 => BotStatusState::Absent,
        _ => return None,
    })
}

/// A decoded `GET /v1/communities/{community}/bot/catalog`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CatalogResponse {
    pub revision: u64,
    pub commands: Vec<BotCommandSpec>,
    pub publication: Option<Vec<u8>>,
    pub status: BotStatus,
}

/// `decodeBotCatalogResponse`: `[revision, commands, publication | null, [state, since | null]]`.
pub fn decode_catalog_response(body: &[u8]) -> Result<CatalogResponse, CatalogError> {
    let v = Value::decode(body).map_err(|_| CatalogError("cbor"))?;
    let [revision, commands, publication, status] = arr(&v, Some(4))? else {
        unreachable!("arity checked")
    };
    let [state, since] = arr(status, Some(2))? else {
        unreachable!("arity checked")
    };
    let publication = match publication {
        Value::Null => None,
        Value::Bytes(p) if !p.is_empty() && p.len() <= MAX_PUBLICATION => Some(p.clone()),
        _ => return Err(CatalogError("publication")),
    };
    Ok(CatalogResponse {
        revision: uint(revision)?,
        commands: decode_commands(arr(commands, None)?)?,
        publication,
        status: BotStatus {
            state: state_of(uint(state)?).ok_or(CatalogError("state"))?,
            since: match since {
                Value::Null => None,
                v => Some(uint(v)?),
            },
        },
    })
}

#[cfg(test)]
mod tests {
    //! Ported from packages/client-core/src/bot/catalog.test.ts (the decoders).
    use super::*;

    fn cmd(name: &str, args: Vec<Value>) -> Value {
        Value::Array(vec![
            Value::Text(name.into()),
            Value::Text("does it".into()),
            Value::Uint(80),
            Value::Array(args),
            Value::Uint(0),
            Value::Uint(0),
            Value::Uint(0),
        ])
    }

    fn arg(name: &str, ty: u64) -> Value {
        Value::Array(vec![
            Value::Text(name.into()),
            Value::Uint(ty),
            Value::Uint(1),
        ])
    }

    #[test]
    fn reads_commands_and_drops_unshowable_names() {
        let got = decode_commands(&[
            cmd("remind", vec![arg("when", 4), arg("text", 0)]),
            cmd("Bad Name", vec![]),
            cmd("ok", vec![arg("Not", 0)]),
        ])
        .unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].name, "remind");
        assert_eq!(got[0].args[0].ty, ArgType::Duration);
    }

    #[test]
    fn refuses_duplicates_bad_types_and_flags() {
        assert!(decode_commands(&[cmd("a", vec![]), cmd("a", vec![])]).is_err());
        assert!(decode_commands(&[cmd("a", vec![arg("x", 6)])]).is_err());
        assert!(decode_commands(&[cmd("a", vec![arg("x", 0), arg("x", 1)])]).is_err());
        let mut bad = cmd("a", vec![]);
        if let Value::Array(items) = &mut bad {
            items[5] = Value::Uint(2);
        }
        assert!(decode_commands(&[bad]).is_err());
    }

    #[test]
    fn reads_a_catalog_response() {
        let body = Value::Array(vec![
            Value::Uint(7),
            Value::Array(vec![cmd("remind", vec![])]),
            Value::Null,
            Value::Array(vec![Value::Uint(2), Value::Uint(1_700_000_000)]),
        ])
        .encode();
        let r = decode_catalog_response(&body).unwrap();
        assert_eq!(r.revision, 7);
        assert_eq!(r.status.state, BotStatusState::Offline);
        assert_eq!(r.status.since, Some(1_700_000_000));
        assert!(r.publication.is_none());
        let bad = Value::Array(vec![
            Value::Uint(7),
            Value::Array(vec![]),
            Value::Null,
            Value::Array(vec![Value::Uint(5), Value::Null]),
        ])
        .encode();
        assert!(decode_catalog_response(&bad).is_err());
    }
}
