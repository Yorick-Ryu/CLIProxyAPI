"""Manual login and automatic recovery intentionally have different preconditions."""
import datetime as dt
import re
from zoneinfo import ZoneInfo
from .security import OpsError

TZ=ZoneInfo('Asia/Shanghai')
AUTH_ERROR=re.compile(r'\b401\b|unauthori[sz]ed|invalidated[ _-]*oauth|authentication_error|token[_ -](?:invalidated|expired|revoked)|refresh_token_reused|invalid_grant',re.I)
TRANSIENT=re.compile(r'\b(?:429|5\d\d)\b|server_is_overloaded|rate.limit|service_unavailable',re.I)


def expiry(note):
    lines=[line for line in str(note or '').splitlines() if re.match(r'^\s*到期时间\s*[:：]',line)]
    if len(lines)!=1:return None
    match=re.fullmatch(r'\s*到期时间\s*[:：]\s*(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2})(?:\s*[（(]北京时间[）)])?\s*',lines[0])
    if not match:return None
    try:return dt.datetime.fromisoformat(' '.join(match.groups())).replace(tzinfo=TZ).timestamp()
    except ValueError:return None


def automatic(row,now,require_failure=True):
    if row.get('disabled') or row.get('status')=='disabled':raise OpsError('disabled',409)
    if str(row.get('plan_type','')).lower()=='free':raise OpsError('free',409)
    end=expiry(row.get('note'))
    if end is None:raise OpsError('expiry_unknown',409)
    if now>=end:raise OpsError('expired',409)
    if not require_failure:return
    message=str(row.get('status_message') or '')
    if TRANSIENT.search(message):raise OpsError('transient',409)
    if not AUTH_ERROR.search(message):raise OpsError('healthy' if row.get('status')=='active' else 'unknown',409)


def periodic_decision(priority,first,repeated):
    # None is a skipped/no-conclusion run; request errors and empty answers are False.
    if first is None or repeated is None:return None
    if priority==0 and first is True and repeated==[True,True]:return 10
    if priority>0 and first is False and repeated==[False,False]:return 0
    return None


def ipv6_candidates(history,occupied,now):
    candidates=[]
    for record in history:
        if record['address'] in occupied or record.get('bound_to'):continue
        if record.get('never_used') is True and record.get('created_at') is not None:rank=0
        else:
            released=record.get('released_at')
            if not isinstance(released,(float,int)) or now-released<86400:continue
            rank=1 if now-released>=172800 else 2
        candidates.append((rank,record['address'],record))
    return [item[2] for item in sorted(candidates,key=lambda item:(item[0],item[1]))]
