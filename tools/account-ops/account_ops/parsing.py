"""Deterministic order parsing; secret fields never belong to preview DTOs."""
import datetime as dt
from decimal import Decimal, InvalidOperation
import re
from zoneinfo import ZoneInfo
from .security import EMAIL, OpsError, email, totp

TZ = ZoneInfo('Asia/Shanghai')
MONEY = re.compile(r'(?:[¥￥]\s*(\d+(?:\.\d{1,2})?)|(?<![\d.])(\d+(?:\.\d{1,2})?)\s*元)')
ORDER = re.compile(r'(?:订单(?:号|编号)?|order(?:\s*id)?)\s*[:：#]?\s*([A-Za-z0-9_-]{3,80})', re.I)
ABSOLUTE = re.compile(r'(\d{4}-\d{2}-\d{2})[ T日\s]+(\d{1,2})[:：](\d{2})')
RELATIVE = re.compile(r'(今天|今晚|明天|后天)\s*(\d{1,2})(?:[:：](\d{2})|点(?:(\d{1,2})分)?)')


def triple(line):
    # Anchor the email and TOTP ends. Delimiters inside the password are data.
    for separator in ('----', '--'):
        match = re.fullmatch(r'\s*(' + EMAIL.pattern + ')' + re.escape(separator) + r'(.+)' + re.escape(separator) + r'([A-Za-z2-7 =]{16,160})\s*', line)
        if match:
            return {'email': email(match[1]), 'password': match[2], 'totp': totp(match[3])}
    raise OpsError('invalid_input')


def deadlines(text, now):
    values = set()
    invalid = False
    for line in text.splitlines():
        if not any(word in line for word in ('到期', '有效', '今晚', '明天', '后天', '今天')):
            continue
        for m in ABSOLUTE.finditer(line):
            try:
                values.add(dt.datetime.fromisoformat(f'{m[1]} {int(m[2]):02}:{m[3]}').replace(tzinfo=TZ).isoformat())
            except ValueError:
                invalid = True
        for m in RELATIVE.finditer(line):
            try:
                day = now.astimezone(TZ).date() + dt.timedelta(days={'今天':0,'今晚':0,'明天':1,'后天':2}[m[1]])
                values.add(dt.datetime.combine(day, dt.time(int(m[2]), int(m[3] or m[4] or 0)), TZ).isoformat())
            except ValueError:
                invalid = True
    return sorted(values), invalid


def parse(text, now):
    if not isinstance(text, str) or not text.strip() or len(text.encode()) > 256_000:
        raise OpsError('invalid_input')
    rows, context, current = [], [], None
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        order = ORDER.search(line)
        if order:
            if current is not None:
                current = None
            context = [line]
            continue
        if EMAIL.search(line) and '--' in line:
            try:
                secrets = triple(line)
            except OpsError:
                # Never echo a malformed triple, even inside an error.
                raise OpsError('invalid_input') from None
            current = {'secrets': secrets, 'context': list(context)}
            rows.append(current)
        elif current is not None:
            current['context'].append(line)
        else:
            context.append(line)
    if not rows:
        rows = [{'secrets': None, 'context': context}]
    if len(rows)>100:raise OpsError('invalid_input')
    result = []
    for index, row in enumerate(rows):
        body = '\n'.join(row.pop('context'))
        orders = sorted(set(ORDER.findall(body)))
        amounts = sorted(set(a or b for a,b in MONEY.findall(body)))
        times, invalid_time = deadlines(body, now)
        issues = []
        if len(orders) > 1: issues.append('order_conflict')
        if len(amounts) > 1: issues.append('amount_conflict')
        if len(times) > 1 or invalid_time: issues.append('expiry_conflict')
        if row['secrets'] is None: issues.append('missing_credentials')
        try:
            amount = int(Decimal(amounts[0]) * 100) if len(amounts) == 1 else None
        except (InvalidOperation, OverflowError):
            raise OpsError('invalid_input') from None
        if amount is not None and amount > 10**12: raise OpsError('invalid_input')
        result.append({**row, 'index': index, 'email': row['secrets']['email'] if row['secrets'] else '',
                       'order_id': orders[0] if len(orders) == 1 else None, 'amount_cents': amount,
                       'expires_at': times[0] if len(times) == 1 else None, 'issues': issues})
    # An order-wide amount cannot silently become a per-account amount.
    counts = {}
    for row in result:
        if row['order_id']: counts[row['order_id']] = counts.get(row['order_id'], 0) + 1
    for row in result:
        if counts.get(row['order_id'], 0) > 1 and row['amount_cents'] is not None:
            row['issues'].append('confirm_per_account_amount')
    return result


def preview(rows):
    return [{k: v for k,v in row.items() if k != 'secrets'} | {'has_credentials': row['secrets'] is not None} for row in rows]
