"""Optional SDK observation metadata never relaxes connection validation."""
from pathlib import Path
from types import SimpleNamespace
import unittest

import provider_test


class ObservationDomainTest(unittest.TestCase):
    setUp = provider_test.ProviderTest.setUp
    call = provider_test.ProviderTest.call
    record = provider_test.ProviderTest.record

    def observed(self, **changes):
        fields = dict(sandbox_id=self.cloud.sandbox_id, sandbox_domain=None,
                      metadata=self.cloud.metadata, template_id=self.cloud.template_id,
                      cpu_count=2, memory_mb=2048, state=self.cloud.state)
        fields.update(changes)
        return SimpleNamespace(**fields)

    def test_create_and_inspect_missing_observation_domain_use_saved_connection(self):
        self.config['Resources'] = {'cpus': 2, 'memory_mib': 2048}
        self.api.get_info.side_effect = lambda *a, **k: self.observed()
        self.assertEqual(self.call('create')['ErrorCode'], '')
        self.assertTrue(self.call('inspect')['Info']['BootstrapComplete'])
        self.assertEqual(self.call('renew')['ErrorCode'], '')
        self.assertEqual(self.record()['connection']['sandbox_domain'], 'e2b.app')
        self.api.connect.assert_not_called()
        self.api.create.assert_called_once()

    def test_paused_inspection_without_domain_does_not_wake(self):
        self.api.get_info.side_effect = lambda *a, **k: self.observed()
        self.assertEqual(self.call('create')['ErrorCode'], '')
        self.cloud.state = 'paused'
        result = self.call('inspect')
        self.assertEqual(result['ErrorCode'], '')
        self.assertEqual(result['Info']['State'], 'paused')
        self.api.connect.assert_not_called()

    def test_unknown_create_list_without_domain_never_recreates_or_attaches(self):
        self.api.create.side_effect = TimeoutError('unknown create')
        self.assertEqual(self.call('create')['ErrorCode'], 'unconfirmed')
        self.api.list.return_value = SimpleNamespace(
            has_next=True, next_items=lambda **kwargs: [self.observed()])
        # One page exhausts the SDK paginator.
        def page(**kwargs):
            self.api.list.return_value.has_next = False
            return [self.observed()]
        self.api.list.return_value.next_items = page
        result = self.call('inspect')
        self.assertEqual(result['ErrorCode'], '')
        self.assertFalse(result['Info']['BootstrapComplete'])
        self.api.create.assert_called_once()
        self.api.connect.assert_not_called()
        self.cloud.files.write.assert_not_called()

    def test_missing_domain_does_not_bypass_identity_or_resource_checks(self):
        self.config['Resources'] = {'cpus': 2, 'memory_mib': 2048}
        for changes, error in (({'metadata': {}}, 'ownership'),
                               ({'template_id': 'other'}, 'invalid'),
                               ({'cpu_count': 4}, 'invalid'),
                               ({'memory_mb': 8192}, 'invalid')):
            with self.subTest(changes=changes):
                for path in Path(self.temporary.name).glob('*.json'):
                    path.unlink()
                self.api.get_info.return_value = self.observed(**changes)
                self.assertEqual(self.call('create')['ErrorCode'], error)
                self.cloud.files.write.assert_not_called()
                self.api.connect.assert_not_called()

    def test_explicit_invalid_observation_domain_rejected_before_credentials(self):
        for domain in ('foreign.example', 'e2b.app.foreign.example', '', 3):
            with self.subTest(domain=domain):
                for path in Path(self.temporary.name).glob('*.json'):
                    path.unlink()
                self.api.get_info.return_value = self.observed(sandbox_domain=domain)
                self.assertEqual(self.call('create')['ErrorCode'], 'ownership')
                self.cloud.files.write.assert_not_called()
                self.api.connect.assert_not_called()

    def test_rejected_connection_cannot_reenter_through_missing_observation_domain(self):
        for domain in ('foreign.example', None):
            with self.subTest(domain=domain):
                for path in Path(self.temporary.name).glob('*.json'):
                    path.unlink()
                self.cloud.sandbox_domain = domain
                self.api.get_info.return_value = self.observed()
                self.assertEqual(self.call('create')['ErrorCode'], 'ownership')
                self.assertEqual(self.call('inspect')['ErrorCode'], 'ownership')
                self.cloud.files.read.assert_not_called()
                self.cloud.files.write.assert_not_called()
                self.api.connect.assert_not_called()

    def test_rejected_observation_cannot_resume_bootstrap_on_later_missing_domain(self):
        self.api.get_info.return_value = self.observed(sandbox_domain='foreign.example')
        self.assertEqual(self.call('create')['ErrorCode'], 'ownership')
        self.assertEqual(self.record()['status'], 'configuration_rejected')
        self.api.get_info.return_value = self.observed()
        result = self.call('inspect')
        self.assertEqual(result['ErrorCode'], '')
        self.assertFalse(result['Info']['BootstrapComplete'])
        self.assertTrue(result['Info']['CreateSettled'])
        self.assertEqual(self.record()['status'], 'configuration_rejected')
        self.cloud.files.read.assert_not_called()
        self.cloud.files.write.assert_not_called()
        self.api.connect.assert_not_called()

    def test_missing_actual_connection_domain_is_still_rejected(self):
        self.api.get_info.return_value = self.observed()
        self.cloud.sandbox_domain = None
        self.assertEqual(self.call('create')['ErrorCode'], 'ownership')
        self.api.get_info.assert_not_called()
        self.cloud.files.write.assert_not_called()
        self.api.connect.assert_not_called()


if __name__ == '__main__':
    unittest.main()
