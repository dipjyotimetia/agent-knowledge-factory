# Evaluation

The factory is successful when a targeted task gets better after a reviewed knowledge change, and previously passing critical tasks do not get worse.

The MVP dataset has 20 banking cases: 10 documentation gaps, 5 contradictions, and 5 negative or no-change cases. Golden answers live outside the curator prompt. The harness checks that a model request body does not contain the golden sentinel.

Metrics and the case file are added with KF-09. The banking loop in KF-10 records before and after for the movement-event dedupe task.
