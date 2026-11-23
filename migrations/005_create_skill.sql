ALTER TABLE proposals DROP CONSTRAINT proposals_decision_chk;
ALTER TABLE proposals ADD CONSTRAINT proposals_decision_chk CHECK (decision IN (
    'update', 'create-skill', 'no-change', 'escalate', 'insufficient-evidence'
));
