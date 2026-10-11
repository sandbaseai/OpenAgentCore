#!/usr/bin/env python3
"""One request over stdin; no service, model calls or provider credentials in logs."""
import time

_HELPER_STARTED = time.monotonic_ns()

import importlib.metadata
import json
import logging
import os
import sys

from provider import Provider
from helper_contract_generated import (SDK_VERSION, PROTOCOL_VERSION, MAX_REQUEST, MAX_RESPONSE,
                                       RESPONSE_FIELDS, ERROR_CODES)
from state import Failure


def valid_response(result):
    return (isinstance(result, dict) and not set(result) - set(RESPONSE_FIELDS) and
            type(result.get('Version')) is int and result['Version'] == PROTOCOL_VERSION and
            result.get('ErrorCode', '') in ERROR_CODES)


def helper_stage(stage, started, completed):
    """Only fixed stages and bounded scalars reach the parent's stderr filter."""
    try:
        sys.stderr.write(json.dumps({'event': 'e2b_helper_stage', 'stage': stage,
                                     'duration_us': (time.monotonic_ns() - started) // 1000,
                                     'completed': completed}) + '\n')
    except Exception:
        pass


def main():
    logging.disable(logging.CRITICAL)
    os.umask(0o077)
    # Private SDK routing selectors may otherwise override the official cloud.
    for key in list(os.environ):
        if key.startswith(('E2B_', 'PYTHON')):
            os.environ.pop(key)
    startup_recorded = False
    try:
        if importlib.metadata.version('e2b') != SDK_VERSION:
            raise Failure('invalid')
        if sys.argv[1:] == ['--check']:
            # Construct the SDK's native transport without sending any request.
            # This also verifies its dynamically imported dependencies survived
            # freezing; importing e2b alone does not exercise that boundary.
            from pyqwest import SyncHTTPTransport
            SyncHTTPTransport(tls_include_system_certs=True)
            helper_stage('helper_startup', _HELPER_STARTED, True)
            startup_recorded = True
            print(json.dumps({'Version': PROTOCOL_VERSION, 'SDKVersion': SDK_VERSION}))
            return
        if len(sys.argv) != 1:
            raise Failure('invalid')
        data = sys.stdin.buffer.read(MAX_REQUEST + 1)
        if len(data) > MAX_REQUEST:
            raise Failure('invalid')
        provider = Provider(json.loads(data))
        helper_stage('helper_startup', _HELPER_STARTED, True)
        startup_recorded = True
        execution_started = time.monotonic_ns()
        completed = False
        try:
            result = provider.execute()
            completed = valid_response(result) and not result.get('ErrorCode', '')
        finally:
            helper_stage('helper_execute', execution_started, completed)
    except Failure as error:
        result = {'Version': PROTOCOL_VERSION, 'ErrorCode': error.code}
    except BaseException:
        result = {'Version': PROTOCOL_VERSION, 'ErrorCode': 'unconfirmed'}
    finally:
        if not startup_recorded:
            helper_stage('helper_startup', _HELPER_STARTED, False)
    output = json.dumps(result) + '\n'
    if not valid_response(result) or len(output.encode('utf-8')) > MAX_RESPONSE:
        output = json.dumps({'Version': PROTOCOL_VERSION, 'ErrorCode': 'unconfirmed'}) + '\n'
    sys.stdout.write(output)


if __name__ == '__main__':
    main()
