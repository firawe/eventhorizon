// Copyright (c) 2014 - The Event Horizon authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repo

import (
	"context"
	"github.com/google/go-cmp/cmp"
	"testing"
	"time"

	eh "github.com/firawe/eventhorizon"
	"github.com/firawe/eventhorizon/mocks"
	"github.com/google/uuid"
)

// AcceptanceTest is the acceptance test that all implementations of Repo
// should pass. It should manually be called from a test case in each
// implementation:
//
//   func TestRepo(t *testing.T) {
//       ctx := context.Background() // Or other when testing namespaces.
//       store := NewRepo()
//       repo.AcceptanceTest(t, ctx, store)
//   }
//

var comparer = cmp.Comparer(func(a, b time.Time) bool {
	if a.UTC().Unix() == b.UTC().Unix() {
		return true
	}
	return false
})

func AcceptanceTest(t *testing.T, ctx context.Context, repo eh.ReadWriteRepo) {
	// Find non-existing item.
	entity, err := repo.Find(ctx, uuid.New().String())
	if rrErr, ok := err.(eh.RepoError); !ok || rrErr.Err != eh.ErrEntityNotFound {
		t.Error("there should be a ErrEntityNotFound error:", err)
	}
	if entity != nil {
		t.Error("there should be no entity:", entity)
	}

	// FindAll with no items.
	result, err := repo.FindAll(ctx)
	if err != nil {
		t.Error("there should be no error:", err)
	}
	if len(result) != 0 {
		t.Error("there should be no items:", len(result))
		t.Errorf("%+v\n", result[0].EntityID())
	}

	// Save model without ID.
	entityMissingID := &mocks.Model{
		Content:   "entity1",
		CreatedAt: time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
	}
	err = repo.Save(ctx, entityMissingID)
	if rrErr, ok := err.(eh.RepoError); !ok || rrErr.BaseErr != eh.ErrMissingEntityID {
		t.Error("there should be a ErrMissingEntityID error:", err)
	}

	// Save and find one item.
	entity1 := &mocks.Model{
		ID:        uuid.New().String(),
		Content:   "entity1",
		CreatedAt: time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
	}
	if err = repo.Save(ctx, entity1); err != nil {
		t.Error("there should be no error:", err)
	}
	entity, err = repo.Find(ctx, entity1.ID)
	if err != nil {
		t.Error("there should be no error:", err)
	}
	if !cmp.Equal(entity, entity1, comparer) {
		t.Error("not equal expected: ", cmp.Diff(entity, entity1, comparer))
	}

	// FindAll with one item.
	result, err = repo.FindAll(ctx)
	if err != nil {
		t.Error("there should be no error:", err)
	}
	if len(result) != 1 {
		t.Error("there should be one item:", len(result))
	}
	if !cmp.Equal(result, []eh.Entity{entity1}, comparer) {
		t.Error("not equal expected: ", cmp.Diff(result, []eh.Entity{entity1}, comparer))
	}

	// Save and overwrite with same ID.
	entity1Alt := &mocks.Model{
		ID:        entity1.ID,
		Content:   "entity1Alt",
		CreatedAt: time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
	}
	if err = repo.Save(ctx, entity1Alt); err != nil {
		t.Error("there should be no error:", err)
	}
	entity, err = repo.Find(ctx, entity1Alt.ID)
	if err != nil {
		t.Error("there should be no error:", err)
	}
	if !cmp.Equal(entity, entity1Alt, comparer) {
		t.Error("not equal expected: ", cmp.Diff(entity, entity1Alt, comparer))
	}

	// Save with another ID.
	entity2 := &mocks.Model{
		ID:        uuid.New().String(),
		Content:   "entity2",
		CreatedAt: time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
	}
	if err = repo.Save(ctx, entity2); err != nil {
		t.Error("there should be no error:", err)
	}
	entity, err = repo.Find(ctx, entity2.ID)
	if err != nil {
		t.Error("there should be no error:", err)
	}

	if !cmp.Equal(entity, entity2, comparer) {
		t.Error("not equal expected: ", cmp.Diff(entity, entity2, comparer))
	}
	// FindAll with two items, order should be preserved from insert.
	result, err = repo.FindAll(ctx)
	if err != nil {
		t.Error("there should be no error:", err)
	}
	if len(result) != 2 {
		t.Error("there should be two items:", len(result))
	}
	if !cmp.Equal(result, []eh.Entity{entity1Alt, entity2}, comparer) {
		t.Error("not equal expected: ", cmp.Diff(result, []eh.Entity{entity1Alt, entity2}, comparer))
	}

	// Remove item.
	if err := repo.Remove(ctx, entity1Alt.ID); err != nil {
		t.Error("there should be no error:", err)
	}
	entity, err = repo.Find(ctx, entity1Alt.ID)
	if rrErr, ok := err.(eh.RepoError); !ok || rrErr.Err != eh.ErrEntityNotFound {
		t.Error("there should be a ErrEntityNotFound error:", err)
	}
	if entity != nil {
		t.Error("there should be no entity:", entity)
	}

	// Remove non-existing item.
	err = repo.Remove(ctx, entity1Alt.ID)
	if rrErr, ok := err.(eh.RepoError); !ok || rrErr.Err != eh.ErrEntityNotFound {
		t.Error("there should be a ErrEntityNotFound error:", err)
	}
}
