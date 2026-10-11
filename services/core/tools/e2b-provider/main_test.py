"""Helper lifecycle diagnostics keep stdout authoritative and stderr bounded."""
import io
import json
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import main
from state import Failure


class HelperTimingTest(unittest.TestCase):
    def run_helper(self, result=None, construction_error=None, execution_error=None, sink=None):
        stdout, stderr = io.StringIO(), sink or io.StringIO()
        with patch('main.sys.stdin', SimpleNamespace(buffer=io.BytesIO(b'{}'))), \
                patch('main.sys.stdout', stdout), patch('main.sys.stderr', stderr), \
                patch('main.sys.argv', ['helper']), patch.dict('main.os.environ', {}, clear=True), \
                patch('main.os.umask'), patch('main.logging.disable'), \
                patch('main.importlib.metadata.version', return_value=main.SDK_VERSION), \
                patch('main._HELPER_STARTED', time.monotonic_ns()), patch('main.Provider') as provider:
            provider.side_effect = construction_error
            provider.return_value.execute.side_effect = execution_error
            provider.return_value.execute.return_value = result or {'Version': main.PROTOCOL_VERSION}
            main.main()
            calls = provider.return_value.execute.call_count
        rows = [json.loads(line) for line in stderr.getvalue().splitlines()] if sink is None else []
        return json.loads(stdout.getvalue()), rows, calls

    def test_startup_execute_and_typed_error_preserve_response(self):
        for error in ('', 'unconfirmed'):
            with self.subTest(error=error):
                expected = {'Version': main.PROTOCOL_VERSION, 'ErrorCode': error}
                output, rows, calls = self.run_helper(result=expected)
                self.assertEqual(output, expected)
                self.assertEqual(calls, 1)
                self.assertEqual([r['stage'] for r in rows], ['helper_startup', 'helper_execute'])
                self.assertEqual([r['completed'] for r in rows], [True, not error])
                for row in rows:
                    self.assertEqual(set(row), {'event', 'stage', 'duration_us', 'completed'})
                    self.assertEqual(row['event'], 'e2b_helper_stage')
                    self.assertGreaterEqual(row['duration_us'], 0)
                    self.assertLessEqual(row['duration_us'], 1_800_000_000)

    def test_failures_emit_only_completed_boundaries_without_error_text(self):
        for phase in ('startup', 'execute'):
            with self.subTest(phase=phase):
                args = {'construction_error': Failure('invalid')} if phase == 'startup' else {'execution_error': RuntimeError('SECRET SDK body')}
                output, rows, calls = self.run_helper(**args)
                self.assertEqual(output['ErrorCode'], 'invalid' if phase == 'startup' else 'unconfirmed')
                self.assertEqual(calls, 0 if phase == 'startup' else 1)
                self.assertEqual(rows[-1]['stage'], 'helper_' + phase)
                self.assertFalse(rows[-1]['completed'])
                self.assertNotIn('SECRET', json.dumps(rows))

    def test_diagnostic_sink_failure_never_replays_or_changes_stdout(self):
        class BrokenSink:
            def write(self, _):
                raise OSError('private sink details')
        output, _, calls = self.run_helper(sink=BrokenSink())
        self.assertEqual(output, {'Version': main.PROTOCOL_VERSION})
        self.assertEqual(calls, 1)

    def test_timing_uses_local_monotonic_clock(self):
        sink = io.StringIO()
        with patch('main.sys.stderr', sink), patch('main.time.monotonic_ns', return_value=2501000):
            main.helper_stage('helper_execute', 1000, True)
        self.assertEqual(json.loads(sink.getvalue())['duration_us'], 2500)
