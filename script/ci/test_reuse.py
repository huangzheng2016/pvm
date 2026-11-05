import copy
import unittest
from reuse import complete_artifacts, reusable_run, trusted_run, RAW_ARTIFACTS

class ReuseTests(unittest.TestCase):
    def setUp(self):
        self.run = dict(id=7,event="push",head_branch="master",head_sha="abc",conclusion="success",status="completed",workflow_id=42,repository={"full_name":"example/pvm"},head_repository={"full_name":"example/pvm"})
    def test_only_successful_same_repository_workflow_commit(self):
        self.assertTrue(trusted_run(self.run,"example/pvm","abc",42))
        for field,value in dict(event="pull_request",head_branch="other",head_sha="def",conclusion="failure",status="in_progress",workflow_id=43,repository={"full_name":"other/pvm"},head_repository={"full_name":"other/pvm"}).items():
            run=copy.deepcopy(self.run);run[field]=value
            self.assertFalse(trusted_run(run,"example/pvm","abc",42),field)
    def test_artifacts_must_all_exist_and_be_unexpired(self):
        artifacts=[dict(name=n,expired=False,size_in_bytes=100) for n in RAW_ARTIFACTS]
        self.assertTrue(complete_artifacts(artifacts))
        self.assertFalse(complete_artifacts(artifacts[:-1]))
        artifacts[0]["expired"]=True
        self.assertFalse(complete_artifacts(artifacts))
    def test_exact_run_with_all_artifacts_is_reused(self):
        def api(path):
            if path=="actions/workflows/build.yml": return {"id":42}
            if "workflow" in path:return {"workflow_runs":[self.run]}
            return {"artifacts":[dict(name=n,expired=False,size_in_bytes=100) for n in RAW_ARTIFACTS]}
        self.assertEqual(reusable_run(api,"example/pvm","abc"),"7")

    def test_missing_artifacts_falls_back(self):
        def api(path):
            if path=="actions/workflows/build.yml": return {"id":42}
            if "workflow" in path:return {"workflow_runs":[self.run]}
            return {"artifacts":[]}
        self.assertEqual(reusable_run(api,"example/pvm","abc"),"")

if __name__ == '__main__': unittest.main()
