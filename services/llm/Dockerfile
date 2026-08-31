FROM python:3.11-slim
WORKDIR /srv
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app ./app
EXPOSE 8002
# --workers 1 is REQUIRED, not a default worth tuning (EVENT_CONTRACTS.md §9.21). Every in-process
# guarantee this service makes is multiplied by the worker count — including the cross-process model
# lease that D-25's enrichment worker and the interactive analyst path both take, and
# PROJECT_PLAN.md §6's one-model-at-a-time 16 GB budget. Raising it does not make the service
# faster; it makes two workers contend for one model. Do not remove this as an optimisation.
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8002", "--workers", "1"]
