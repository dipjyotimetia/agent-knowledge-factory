# Evaluation

The factory is successful when a targeted task gets better after a reviewed knowledge change, and previously passing critical tasks do not get worse.

The MVP dataset is `examples/banking/tests/cases.json`: 10 documentation gaps, 5 contradictions, and 5 negative cases. Golden answers and sentinels stay out of `evaluations.CuratorPrompt`. On the stale fixture the critical dedupe case fails. After the reviewed edit it passes, and critical cases that already passed must still pass.

Metrics and the case file are added with KF-09. The banking loop in KF-10 records before and after for the movement-event dedupe task.
