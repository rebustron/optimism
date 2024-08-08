package proposer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/ava-labs/coreth/accounts/abi/bind"
	"github.com/ethereum-optimism/optimism/op-proposer/proposer/db/ent"
	"github.com/ethereum-optimism/optimism/op-proposer/proposer/db/ent/proofrequest"
)

func (l *L2OutputSubmitter) ProcessPendingProofs() error {
	reqs, err := l.db.GetAllPendingProofs()
	if err != nil {
		return err
	}
	for _, req := range reqs {
		status, proof, err := l.GetProofStatus(req.ProverRequestID)
		if status == "PROOF_FULFILLED" {
			// update the proof to the DB and update status to "COMPLETE"
			err = l.db.AddProof(req.ID, proof)
			if err != nil {
				l.Log.Error("failed to update completed proof status", "err", err)
				return err
			}
		}

		timeout := time.Now().Unix() > req.ProofRequestTime+l.DriverSetup.Cfg.MaxProofTime
		// ZTODO: Talk to Succinct about logic of different statuses
		if timeout {
			// update status in db to "FAILED"
			err = l.db.UpdateProofStatus(req.ID, "FAILED")
			if err != nil {
				l.Log.Error("failed to update failed proof status", "err", err)
				return err
			}

			// If an AGG proof failed, we're in trouble.
			// Try again.
			if req.Type == proofrequest.TypeAGG {
				l.Log.Error("failed to get agg proof, adding to db to retry", "req", req)

				err = l.db.NewEntry("AGG", req.StartBlock, req.EndBlock)
				if err != nil {
					l.Log.Error("failed to add new proof request", "err")
					return err
				}
			}

			// If a SPAN proof failed, assume it was too big.
			// Therefore, create two new entries for the original proof split in half.
			tmpStart := req.StartBlock
			tmpEnd := tmpStart + ((req.EndBlock - tmpStart) / 2)
			for i := 0; i < 2; i++ {
				err = l.db.NewEntry("SPAN", tmpStart, tmpEnd)
				if err != nil {
					l.Log.Error("failed to add new proof request", "err", err)
					return err
				}

				tmpStart = tmpEnd + 1
				tmpEnd = req.EndBlock
			}
		}
	}

	return nil
}

func (l *L2OutputSubmitter) RequestQueuedProofs(ctx context.Context) error {
	unrequestedProofs, err := l.db.GetAllUnrequestedProofs()
	if err != nil {
		return fmt.Errorf("failed to get unrequested proofs: %w", err)
	}

	for _, proof := range unrequestedProofs {
		if proof.Type == proofrequest.TypeAGG {
			blockNumber, blockHash, err := l.checkpointBlockHash(ctx)
			if err != nil {
				l.Log.Error("failed to checkpoint block hash", "err", err)
				return err
			}
			l.db.AddL1BlockInfo(proof.StartBlock, proof.EndBlock, blockNumber, blockHash)
		}
		go func(p ent.ProofRequest) {
			err = l.db.UpdateProofStatus(proof.ID, "REQ")
			if err != nil {
				l.Log.Error("failed to update proof status", "err", err)
				return
			}

			err = l.RequestKonaProof(p)
			if err != nil {
				err = l.db.UpdateProofStatus(proof.ID, "FAILED")
				if err != nil {
					l.Log.Error("failed to revert proof status", "err", err, "proverRequestID", proof.ID)
				}
				l.Log.Error("failed to request proof from Kona SP1", "err", err, "proof", p)
			}
		}(proof)
	}

	return nil
}

// Use the L2OO contract to look up the range of blocks that the next proof must cover.
// Check the DB to see if we have sufficient span proofs to request an agg proof that covers this range.
// If so, queue up the agg proof in the DB to be requested later.
func (l *L2OutputSubmitter) DeriveAggProofs(ctx context.Context) error {
	latest, err := l.l2ooContract.LatestOutputIndex(&bind.CallOpts{Context: ctx})
	if err != nil {
		return fmt.Errorf("failed to get latest L2OO output: %w", err)
	}
	from := latest.Uint64() + 1

	minTo, err := l.l2ooContract.NextOutputIndex(&bind.CallOpts{Context: ctx})
	if err != nil {
		return fmt.Errorf("failed to get next L2OO output: %w", err)
	}

	_, err = l.db.TryCreateAggProofFromSpanProofs(from, minTo.Uint64())
	if err != nil {
		return fmt.Errorf("failed to create agg proof from span proofs: %w", err)
	}

	return nil
}

func (l *L2OutputSubmitter) RequestKonaProof(p ent.ProofRequest) error {
	prevConfirmedBlock := p.StartBlock - 1
	var proofId string
	var err error

	if p.Type == proofrequest.TypeAGG {
		proofId, err = l.RequestAggProof(prevConfirmedBlock, p.EndBlock)
		if err != nil {
			return fmt.Errorf("failed to request AGG proof: %w", err)
		}
	} else if p.Type == proofrequest.TypeSPAN {
		proofId, err = l.RequestSpanProof(prevConfirmedBlock, p.EndBlock)
		if err != nil {
			return fmt.Errorf("failed to request SPAN proof: %w", err)
		}
	} else {
		return fmt.Errorf("unknown proof type: %d", p.Type)
	}

	err = l.db.SetProverRequestID(p.ID, proofId)
	if err != nil {
		return fmt.Errorf("failed to set prover request ID: %w", err)
	}

	return nil
}

type SpanProofRequest struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

type AggProofRequest struct {
	Subproofs [][]byte `json:"subproofs"`
}

type ProofResponse struct {
	ProofID string `json:"id"`
}

func (l *L2OutputSubmitter) RequestSpanProof(start, end uint64) (string, error) {
	requestBody := SpanProofRequest{
		Start: start,
		End:   end,
	}
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	return l.RequestProofFromServer(jsonBody)
}

func (l *L2OutputSubmitter) RequestAggProof(start, end uint64) (string, error) {
	subproofs, err := l.db.GetSubproofs(start, end)
	if err != nil {
		return "", fmt.Errorf("failed to get subproofs: %w", err)
	}
	requestBody := AggProofRequest{
		Subproofs: subproofs,
	}
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	return l.RequestProofFromServer(jsonBody)
}

func (l *L2OutputSubmitter) RequestProofFromServer(jsonBody []byte) (string, error) {
	req, err := http.NewRequest("POST", l.DriverSetup.Cfg.KonaServerUrl, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Errorf("Error reading the response body: %v", err)
	}

	// Create a variable of the Response type
	var response ProofResponse

	// Unmarshal the JSON into the response variable
	err = json.Unmarshal(body, &response)
	if err != nil {
		fmt.Errorf("Error decoding JSON response: %v", err)
	}

	return response.ProofID, nil
}

type ProofStatus struct {
	Status string `json:"status"`
	Proof  []byte `json:"proof"`
}

func (l *L2OutputSubmitter) GetProofStatus(proofId string) (string, []byte, error) {
	req, err := http.NewRequest("GET", l.DriverSetup.Cfg.KonaServerUrl+"/status/"+proofId, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Errorf("Error reading the response body: %v", err)
	}

	// Create a variable of the Response type
	var response ProofStatus

	// Unmarshal the JSON into the response variable
	err = json.Unmarshal(body, &response)
	if err != nil {
		fmt.Errorf("Error decoding JSON response: %v", err)
	}

	return response.Status, response.Proof, nil
}
